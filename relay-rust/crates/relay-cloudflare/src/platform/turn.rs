use super::access;
use crate::turn_credentials::{Key, select};
use futures_util::StreamExt;
use js_sys::Uint8Array;
use serde_json::{Value, json};
use wasm_bindgen::JsCast;
use windshare_relay_access::Digest;
use worker::{
    Env, Fetch, Headers, Method, Request, RequestInit, RequestRedirect, Response, ResponseBody,
    Result,
};

const TTL_SECONDS: u32 = 1800;
const UPSTREAM_TIMEOUT_MS: u32 = 15_000;
const MAX_UPSTREAM_BYTES: usize = 32 * 1024;

async fn upstream_json(response: &Response) -> Option<Value> {
    let ResponseBody::Stream(stream) = response.body() else {
        return None;
    };
    let mut chunks = wasm_streams::ReadableStream::from_raw(stream.clone())
        .try_into_stream()
        .ok()?;
    let mut bytes = Vec::new();
    while let Some(item) = chunks.next().await {
        // Check the JS buffer before copying it into Wasm. The stream cancels on early return.
        let chunk = item.ok()?.dyn_into::<Uint8Array>().ok()?;
        let start = bytes.len();
        let length = chunk.length() as usize;
        if length > MAX_UPSTREAM_BYTES - start {
            return None;
        }
        bytes.resize(start + length, 0);
        chunk.copy_to(&mut bytes[start..]);
    }
    serde_json::from_slice(&bytes).ok()
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
    if !key.valid() {
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
    let Ok(response) = Fetch::Request(request).send_with_signal(&timeout).await else {
        return refused("turn_unavailable", 503);
    };
    if !(200..300).contains(&response.status_code()) {
        return refused("turn_unavailable", 503);
    }
    let Some(body) = upstream_json(&response).await else {
        return refused("turn_unavailable", 503);
    };
    let Some(credential) = select(&body["iceServers"]) else {
        return refused("turn_unavailable", 503);
    };
    worker::console_log!(
        "{}",
        json!({"event":"turn_credentials_issued","url_count":credential.urls.len(),"ttl_seconds":TTL_SECONDS})
    );
    access::reply(
        &json!({"urls":credential.urls,"username":credential.username,"password":credential.password,"ttl_seconds":TTL_SECONDS}),
        200,
    )
}
