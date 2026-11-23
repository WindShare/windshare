use super::access;
use serde::Deserialize;
use serde_json::{Value, json};
use windshare_relay_access::Digest;
use worker::{
    Env, Fetch, Headers, Method, Request, RequestInit, RequestRedirect, Response, Result,
};

const TTL_SECONDS: u32 = 1800;
const UPSTREAM_TIMEOUT_MS: u32 = 15_000;
const MAX_URLS: usize = 8;
const MAX_CREDENTIAL: usize = 1024;
#[derive(Deserialize)]
struct Key {
    key_id: String,
    api_token: String,
}
#[derive(Deserialize)]
struct Server {
    urls: Vec<String>,
    #[serde(default)]
    username: String,
    #[serde(default)]
    credential: String,
}
fn valid_url(raw: &str) -> bool {
    let Some((scheme, tail)) = raw.split_once(':') else {
        return false;
    };
    if !matches!(scheme, "turn" | "turns")
        || raw.len() > 512
        || tail.contains(['@', '#', '/', '\\', '%'])
        || tail.chars().any(char::is_whitespace)
    {
        return false;
    }
    let host = match tail.split_once('?') {
        None => tail,
        Some((host, "transport=udp" | "transport=tcp")) => host,
        Some(_) => return false,
    };
    !host.is_empty()
        && !host.ends_with(':')
        && url::Url::parse(&format!("https://{host}"))
            .is_ok_and(|u| u.host().is_some() && u.port() != Some(0))
}
fn refused(reason: &str, status: u16) -> Result<Response> {
    worker::console_log!(
        "{}",
        json!({"event":"turn_credentials_refused","reason":reason,"status":status})
    );
    access::reply(&json!({"error":reason}), status)
}
pub(super) async fn issue(env: &Env, req: &Request) -> Result<Response> {
    if req.method() != Method::Post {
        return refused("method_not_allowed", 405);
    }
    let Ok(secret) = env.secret("TURN_KEY") else {
        return refused("turn_unavailable", 404);
    };
    let Some(header) = req.headers().get("Authorization")? else {
        return refused("access_denied", 403);
    };
    let Some(bearer) = header.strip_prefix("Bearer ").filter(|v| !v.is_empty()) else {
        return refused("access_denied", 403);
    };
    let digest = windshare_relay_console::text_id(&Digest::of(bearer.as_bytes()).0);
    let check = Request::new_with_init(
        "https://access/internal/authorize-turn",
        RequestInit::new()
            .with_method(Method::Post)
            .with_body(Some(json!({"digest":digest}).to_string().into())),
    )?;
    let allowed = access::fetch(env, check).await?;
    if allowed.status_code() != 200 {
        worker::console_log!(
            "{}",
            json!({"event":"turn_credentials_refused","status":allowed.status_code()})
        );
        return Ok(allowed);
    }
    let Ok(key) = serde_json::from_str::<Key>(&secret.to_string()) else {
        return refused("turn_unavailable", 503);
    };
    if key.key_id.is_empty()
        || !key
            .key_id
            .bytes()
            .all(|c| c.is_ascii_alphanumeric() || c == b'-')
    {
        return refused("turn_unavailable", 503);
    }
    let headers = Headers::new();
    headers.set("Authorization", &format!("Bearer {}", key.api_token))?;
    headers.set("Content-Type", "application/json")?;
    let request = Request::new_with_init(
        &format!(
            "https://rtc.live.cloudflare.com/v1/turn/keys/{}/credentials/generate-ice-servers",
            key.key_id
        ),
        RequestInit::new()
            .with_method(Method::Post)
            .with_headers(headers)
            .with_redirect(RequestRedirect::Manual)
            .with_body(Some(json!({"ttl":TTL_SECONDS}).to_string().into())),
    )?;
    let timeout = worker::web_sys::AbortSignal::timeout_with_u32(UPSTREAM_TIMEOUT_MS).into();
    let Ok(mut response) = Fetch::Request(request).send_with_signal(&timeout).await else {
        return refused("turn_unavailable", 503);
    };
    if !(200..300).contains(&response.status_code()) {
        return refused("turn_unavailable", 503);
    }
    let Ok(mut body) = response.json::<Value>().await else {
        return refused("turn_unavailable", 503);
    };
    let servers = body["iceServers"].take();
    let entries = match servers {
        Value::Array(entries) => entries,
        Value::Object(_) => vec![servers],
        _ => return refused("turn_unavailable", 503),
    };
    for entry in entries {
        let Ok(s) = serde_json::from_value::<Server>(entry) else {
            continue;
        };
        let urls: Vec<_> = s
            .urls
            .into_iter()
            .filter(|u| valid_url(u))
            .take(MAX_URLS)
            .collect();
        if urls.is_empty()
            || s.username.is_empty()
            || s.credential.is_empty()
            || s.username.len() > MAX_CREDENTIAL
            || s.credential.len() > MAX_CREDENTIAL
        {
            continue;
        }
        worker::console_log!(
            "{}",
            json!({"event":"turn_credentials_issued","url_count":urls.len(),"ttl_seconds":TTL_SECONDS})
        );
        return access::reply(
            &json!({"urls":urls,"username":s.username,"password":s.credential,"ttl_seconds":TTL_SECONDS}),
            200,
        );
    }
    refused("turn_unavailable", 503)
}
