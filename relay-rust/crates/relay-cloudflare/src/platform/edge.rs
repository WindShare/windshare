use super::access;
use crate::config::Route;
use serde_json::json;
use windshare_relay_access::Digest;
use worker::{Context, Env, Method, Request, Response, Result, event};

pub const NAMESPACE: &str = "SHARES";

#[event(fetch)]
pub async fn fetch(req: Request, env: Env, _ctx: Context) -> Result<Response> {
    let origin = req.url()?.origin().ascii_serialization();
    let console = env
        .var("CONSOLE_ORIGIN")
        .ok()
        .map(|value| value.to_string());
    if access::turn_endpoint(&req.path()) {
        return access::fetch(&env, req).await;
    }
    if console.as_deref() == Some(origin.as_str()) && !req.path().starts_with("/internal/") {
        let path = req.path();
        if !path.starts_with("/api/") {
            // Serve embedded console pages and hashed bundles directly from the
            // stateless edge Worker so static traffic and probes never wake the
            // singleton AccessObject Durable Object or open SQLite.
            return if req.method() == Method::Get {
                console_asset(&path)
            } else {
                Response::error("method not allowed", 405)
            };
        }
        return access::fetch(&env, req).await;
    }
    if req.method() == Method::Get && req.path() == "/.well-known/windshare" {
        let mut response = Response::from_json(&json!({
            "version":1,"access":"restricted",
            "relay_url":env.var("RELAY_URL").map_or(origin,|value| value.to_string()),
            "console_url":console,"frontend_url":env.var("FRONTEND_URL").ok().map(|value| value.to_string()),
        }))?;
        response.headers_mut().set(
            "Cache-Control",
            windshare_relay_console::NO_STORE_CACHE_CONTROL,
        )?;
        return Ok(response);
    }
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
    let digest = req.headers().get("Authorization")?.and_then(|header| {
        header
            .strip_prefix("Bearer ")
            .map(|key| access::text_id(&Digest::of(key.as_bytes()).0))
    });
    let mut forwarded = req.clone_mut()?;
    // Only a digest crosses the edge. It identifies a credential; it never caches permission.
    forwarded.headers_mut()?.delete(access::CREDENTIAL_HEADER)?;
    if let Some(digest) = digest {
        forwarded
            .headers_mut()?
            .set(access::CREDENTIAL_HEADER, &digest)?;
    }
    forwarded.headers_mut()?.delete("Authorization")?;
    env.durable_object(NAMESPACE)?
        .id_from_name(&route.object_name())?
        .get_stub()?
        .fetch_with_request(forwarded)
        .await
}

fn console_asset(path: &str) -> Result<Response> {
    let Some((content, kind)) = windshare_relay_console::asset(path) else {
        return Response::error("not found", 404);
    };
    let mut response = Response::ok(content)?;
    for (name, value) in [
        ("Content-Type", kind),
        (
            "Cache-Control",
            windshare_relay_console::asset_cache_control(path),
        ),
        ("X-Content-Type-Options", "nosniff"),
        ("Content-Security-Policy", windshare_relay_console::POLICY),
        ("Referrer-Policy", "no-referrer"),
        ("Cross-Origin-Opener-Policy", "same-origin"),
    ] {
        response.headers_mut().set(name, value)?;
    }
    Ok(response)
}
