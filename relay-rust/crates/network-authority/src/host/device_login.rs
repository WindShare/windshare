use super::{WorkerEnvironment, failure, storage::Database};
use crate::{Authority, Fault, PasswordVerifier};
use anyhow::{Result, ensure};
use serde::Deserialize;
use worker::Response;

pub(super) const COOKIE: &str = "windshare_service_login=";

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct DeviceLogin {
    username: String,
    password_verifier: PasswordVerifier,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Request {
    invitation: String,
    login: Option<DeviceLogin>,
    username: Option<String>,
}

pub(super) fn handle(
    authority: &mut Authority<Database, WorkerEnvironment>,
    path: &str,
    source: &str,
    cookie: &str,
    bytes: &[u8],
) -> Result<Response> {
    let body: Request = serde_json::from_slice(bytes).map_err(|_| Fault::Invalid)?;
    if path == "/v1/device/preview" {
        ensure!(
            body.login.is_none() && body.username.is_none(),
            Fault::Invalid
        );
        let invite = authority.device_invitation(&body.invitation)?;
        let current = authority.browser_account(cookie);
        let preview = serde_json::json!({
            "origin": invite.origin,
            "device": invite.device,
            "code": invite.code,
            "username": current.and_then(|account| account.username),
        });
        return Ok(Response::from_json(&preview).map_err(failure)?);
    }
    if let Some(login) = body.login {
        let approved = authority.device_sign_in(
            source,
            &body.invitation,
            &login.username,
            &login.password_verifier,
        )?;
        return cookie_response(&approved.origin, &approved.approval, &approved.session);
    }
    let invite = authority.device_invitation(&body.invitation)?;
    let account = authority
        .browser_account(cookie)
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

fn cookie_response(origin: &str, approval: &str, session: &str) -> Result<Response> {
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
