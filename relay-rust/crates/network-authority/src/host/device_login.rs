use super::{WorkerEnvironment, failure, storage::Database};
use crate::{Authority, Fault};
use anyhow::{Result, ensure};
use serde::Deserialize;
use worker::Response;

pub(super) const COOKIE: &str = "windshare_service_login=";
#[derive(Deserialize)]
struct Request {
    invitation: String,
    login: Option<serde_json::Value>,
    username: Option<String>,
}

pub(super) fn handle(
    authority: &mut Authority<Database, WorkerEnvironment>,
    path: &str,
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
    // Password logins are verified on the stateless edge Worker via /internal/device/login/*.
    ensure!(body.login.is_none(), Fault::Invalid);
    let account = current
        .filter(|account| account.username.is_some() && account.username == body.username)
        .ok_or_else(|| Fault::Denied {
            reason: "session_changed".into(),
        })?;
    let approval = authority.approve_device(&invite, &account)?;
    Ok(
        Response::from_json(&serde_json::json!({"origin":invite.origin,"approval":approval}))
            .map_err(failure)?,
    )
}

pub(super) fn cookie_response(origin: &str, approval: &str, session: &str) -> Result<Response> {
    let mut response =
        Response::from_json(&serde_json::json!({"origin":origin,"approval":approval}))
            .map_err(failure)?;
    response
        .headers_mut()
        .set(
            "Set-Cookie",
            &format!("{COOKIE}{session}; HttpOnly; Secure; SameSite=Strict; Path=/v1/device"),
        )
        .map_err(failure)?;
    Ok(response)
}
