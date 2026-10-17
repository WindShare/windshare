use crate::config::Route;
use worker::{Context, Env, Method, Request, Response, Result, event};

pub const NAMESPACE: &str = "SHARES";
pub const REGISTER_HEADER: &str = "x-windshare-register";
pub const REGISTER_TAG: &str = "registration-permitted";
pub const REGISTRATION_SECRET: &str = "REGISTRATION_SECRET";

#[event(fetch)]
pub async fn fetch(req: Request, env: Env, _ctx: Context) -> Result<Response> {
    if req.method() != Method::Get {
        return Response::error("GET required", 405);
    }
    let Ok(route) = Route::parse(req.url()?.as_str()) else {
        return Response::error("invalid relay route", 400);
    };
    if !req
        .headers()
        .get("Upgrade")?
        .is_some_and(|value| value.eq_ignore_ascii_case("websocket"))
    {
        return Response::error("WebSocket upgrade required", 426);
    }
    let permission = registration_permission(&req, &env)?;
    let mut forwarded = req.clone_mut()?;
    // Overwrite caller-controlled headers; only the edge can grant this capability.
    forwarded
        .headers_mut()?
        .set(REGISTER_HEADER, if permission { "1" } else { "0" })?;
    forwarded.headers_mut()?.delete("Authorization")?;
    let namespace = env.durable_object(NAMESPACE)?;
    namespace
        .id_from_name(&route.object_name())?
        .get_stub()?
        .fetch_with_request(forwarded)
        .await
}

fn registration_permission(req: &Request, env: &Env) -> Result<bool> {
    let supplied = req.headers().get("Authorization")?;
    let configured = env
        .secret(REGISTRATION_SECRET)
        .ok()
        .map(|secret| secret.to_string());
    // Absence is fail-closed for REGISTER. Signed STOP and token-bound RESUME
    // still reach the object, independently of registration-credential rotation.
    Ok(match (supplied, configured) {
        (Some(header), Some(secret)) if !secret.is_empty() => header == format!("Bearer {secret}"),
        _ => false,
    })
}
