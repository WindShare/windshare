use super::{failure, required};
use crate::turn::{Grant, LEASE_LIFETIME_MS};
use serde::Deserialize;
use serde_json::{Value, json};
use worker::{Env, Fetch, Headers, Method, Request, RequestInit, Response, Result};

const PROVIDER_BASE: &str = "https://rtc.live.cloudflare.com/v1/turn/keys/";
const MAX_RESPONSE_BYTES: usize = 64 * 1024;
const REQUEST_TIMEOUT_MS: u32 = 15_000;
const MAX_URL_BYTES: usize = 512;
const MAX_SECRET_BYTES: usize = 1024;
const MAX_URLS: usize = 8;

#[derive(Deserialize)]
pub(super) struct Secret {
    pub key_id: String,
    api_token: String,
}
pub(super) fn secret(env: &Env) -> Result<Secret> {
    let secret: Secret = serde_json::from_str(&env.secret("TURN_KEY")?.to_string())?;
    if secret.key_id.is_empty()
        || !secret
            .key_id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric())
        || secret.api_token.is_empty()
    {
        return Err(failure("invalid TURN key binding"));
    }
    Ok(secret)
}
async fn request(secret: &Secret, suffix: &str, body: Option<Value>) -> Result<Response> {
    let headers = Headers::new();
    headers.set("Authorization", &format!("Bearer {}", secret.api_token))?;
    headers.set("Content-Type", "application/json")?;
    let request = Request::new_with_init(
        &format!("{PROVIDER_BASE}{}{suffix}", secret.key_id),
        RequestInit::new()
            .with_method(Method::Post)
            .with_redirect(worker::RequestRedirect::Manual)
            .with_headers(headers)
            .with_body(body.map(|value| value.to_string().into())),
    )?;
    let signal =
        worker::AbortSignal::from(web_sys::AbortSignal::timeout_with_u32(REQUEST_TIMEOUT_MS));
    Fetch::Request(request).send_with_signal(&signal).await
}
pub(super) async fn issue(secret: &Secret) -> Result<Grant> {
    let mut response = request(
        secret,
        "/credentials/generate-ice-servers",
        Some(json!({"ttl":LEASE_LIFETIME_MS / 1000})),
    )
    .await?;
    if !matches!(response.status_code(), 200 | 201) {
        return Err(failure(format!(
            "TURN provider issuance HTTP {}",
            response.status_code()
        )));
    }
    let body = response.bytes().await?;
    if body.len() > MAX_RESPONSE_BYTES {
        return Err(failure("TURN provider response exceeds limit"));
    }
    let value: Value = serde_json::from_slice(&body)?;
    let servers = value
        .get("iceServers")
        .and_then(Value::as_array)
        .ok_or_else(|| failure("invalid TURN provider response"))?;
    let mut grant = Grant {
        urls: Vec::new(),
        username: String::new(),
        password: String::new(),
    };
    for server in servers {
        let urls = server
            .get("urls")
            .and_then(Value::as_array)
            .ok_or_else(|| failure("invalid TURN URLs"))?;
        for raw in urls {
            let url = raw.as_str().ok_or_else(|| failure("invalid TURN URL"))?;
            if url.starts_with("stun:") {
                continue;
            }
            if !(url.starts_with("turn:") || url.starts_with("turns:"))
                || url.len() > MAX_URL_BYTES
                || url.chars().any(char::is_whitespace)
                || grant.urls.len() >= MAX_URLS
                || grant.urls.iter().any(|existing| existing == url)
            {
                return Err(failure("invalid TURN URL"));
            }
            let username = required(server, "username")?;
            let password = required(server, "credential")?;
            if username.is_empty()
                || password.is_empty()
                || username.len() > MAX_SECRET_BYTES
                || password.len() > MAX_SECRET_BYTES
                || (!grant.username.is_empty()
                    && (grant.username != username || grant.password != password))
            {
                return Err(failure("invalid TURN credentials"));
            }
            grant.username = username.to_string();
            grant.password = password.to_string();
            grant.urls.push(url.to_string());
        }
    }
    if grant.urls.is_empty() {
        return Err(failure("TURN provider returned no endpoints"));
    }
    Ok(grant)
}
pub(super) async fn revoke(secret: &Secret, username: &str) -> Result<()> {
    let encoded: String = url::form_urlencoded::byte_serialize(username.as_bytes()).collect();
    let response = request(secret, &format!("/credentials/{encoded}/revoke"), None).await?;
    if response.status_code() != 204 {
        return Err(failure(format!(
            "TURN provider revocation HTTP {}",
            response.status_code()
        )));
    }
    Ok(())
}
