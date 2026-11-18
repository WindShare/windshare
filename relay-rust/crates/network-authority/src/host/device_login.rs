use super::{Clock, failure, storage::Database};
use crate::{Authority, Fault};
use anyhow::Result;
use serde::Deserialize;
use worker::Response;

pub(super) const COOKIE: &str = "windshare_service_login=";
#[derive(Deserialize)]
struct Request {
    invitation: String,
    login: Option<Login>,
    username: Option<String>,
}
#[derive(Deserialize)]
struct Login {
    username: String,
    password: String,
}

pub(super) fn handle(
    authority: &mut Authority<Database, Clock>,
    path: &str,
    source: &str,
    cookie: &str,
    bytes: &[u8],
) -> Result<Response> {
    let body: Request = serde_json::from_slice(bytes).map_err(|_| Fault::Invalid)?;
    let invite = authority.device_invitation(&body.invitation)?;
    let current = authority.browser_account(cookie);
    if path == "/v1/device/preview" {
        let preview = serde_json::json!({
            "origin": invite.origin,
            "device": invite.device,
            "code": invite.code,
            "username": current.and_then(|account| account.username),
        });
        return Ok(Response::from_json(&preview).map_err(failure)?);
    }
    let (account, session) = if let Some(login) = body.login {
        let (account, session) =
            authority.device_sign_in(source, &login.username, &login.password)?;
        (account, Some(session))
    } else {
        (
            current
                .filter(|account| account.username.is_some() && account.username == body.username)
                .ok_or_else(|| Fault::Denied {
                    reason: "session_changed".into(),
                })?,
            None,
        )
    };
    let approval = authority.approve_device(&invite, &account)?;
    let mut response =
        Response::from_json(&serde_json::json!({"origin":invite.origin,"approval":approval}))
            .map_err(failure)?;
    if let Some(session) = session {
        response
            .headers_mut()
            .set(
                "Set-Cookie",
                &format!("{COOKIE}{session}; HttpOnly; Secure; SameSite=Strict; Path=/v1/device"),
            )
            .map_err(failure)?;
    }
    Ok(response)
}
