use serde_json::json;
use worker::{Context, Env, Method, Request, Response, Result, event};

const POLICY: &str = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'";
#[event(fetch)]
pub async fn fetch(request: Request, env: Env, _context: Context) -> Result<Response> {
    let origin = request.headers().get("Origin")?;
    let own = request.url()?.origin().ascii_serialization();
    if !allowed_origin(&request.path(), origin.as_deref(), &own, &env) {
        return Response::error("origin refused", 403);
    }
    let mut response = if request.method() == Method::Options {
        Response::empty()?.with_status(204)
    } else if request.method() == Method::Get {
        match request.path().as_str() {
            "/" | "/console" => asset(
                include_str!("../../console/index.html"),
                "text/html; charset=utf-8",
            )?,
            "/console.js" => asset(
                include_str!("../../console/console.js"),
                "text/javascript; charset=utf-8",
            )?,
            "/console.css" => asset(
                include_str!("../../console/console.css"),
                "text/css; charset=utf-8",
            )?,
            "/report" => asset(
                include_str!("../../console/report.html"),
                "text/html; charset=utf-8",
            )?,
            "/report.js" => asset(
                include_str!("../../console/report.js"),
                "text/javascript; charset=utf-8",
            )?,
            "/healthz" => Response::from_json(&json!({"status":"ok"}))?,
            _ => forward(request, &env).await?,
        }
    } else {
        if request.path().starts_with("/v1/admin/") && origin.is_none() {
            return Response::error("administrator mutations require an origin", 403);
        }
        if request.path() != "/v1/node/usage"
            && request
                .headers()
                .get("Content-Type")?
                .is_none_or(|value| !value.starts_with("application/json"))
        {
            return Response::error("JSON required", 415);
        }
        forward(request, &env).await?
    };
    // Durable Object fetch responses have immutable headers; preserve the body
    // and cookies while taking ownership of the edge response policy.
    let headers = response.headers().clone();
    response = response.with_headers(headers);
    response.headers_mut().set("Cache-Control", "no-store")?;
    let policy = env.var("DIRECTORY_ORIGIN").map_or_else(
        |_| POLICY.to_owned(),
        |origin| {
            POLICY.replace(
                "connect-src 'self'",
                &format!("connect-src 'self' {origin}"),
            )
        },
    );
    response
        .headers_mut()
        .set("Content-Security-Policy", &policy)?;
    response
        .headers_mut()
        .set("X-Content-Type-Options", "nosniff")?;
    response
        .headers_mut()
        .set("Referrer-Policy", "no-referrer")?;
    if let Some(origin) = origin {
        response
            .headers_mut()
            .set("Access-Control-Allow-Origin", &origin)?;
        response
            .headers_mut()
            .set("Access-Control-Allow-Credentials", "true")?;
        response.headers_mut().set(
            "Access-Control-Allow-Headers",
            "Content-Type, Authorization, X-CSRF-Token",
        )?;
        response
            .headers_mut()
            .set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")?;
        response.headers_mut().set("Vary", "Origin")?;
    }
    Ok(response)
}
fn asset(body: &str, kind: &str) -> Result<Response> {
    let mut response = Response::ok(body)?;
    response.headers_mut().set("Content-Type", kind)?;
    Ok(response)
}
async fn forward(request: Request, env: &Env) -> Result<Response> {
    let peer = request.headers().get("CF-Connecting-IP")?;
    let forwarded = request.headers().get("X-Windshare-Source-Ip")?;
    let trusted = env
        .var("TRUSTED_PROXY_IPS")
        .map_or_else(|_| String::new(), |value| value.to_string());
    let source = crate::source::source_key(peer.as_deref(), forwarded.as_deref(), &trusted)
        .map_err(super::failure)?;
    let mut request = request.clone_mut()?;
    request.headers_mut()?.set("X-Authority-Source", &source)?;
    env.durable_object("AUTHORITY")?
        .id_from_name("network-authority-v1")?
        .get_stub()?
        .fetch_with_request(request)
        .await
}

fn allowed_origin(path: &str, origin: Option<&str>, own: &str, env: &Env) -> bool {
    let admin = path.starts_with("/v1/admin/");
    let api_origin = env.var("API_ORIGIN").ok().map(|v| v.to_string());
    if admin && origin.is_some_and(|v| v != own && Some(v) != api_origin.as_deref()) {
        return false;
    }
    origin.is_none_or(|origin| {
        origin == own
            || env.var("ALLOWED_ORIGINS").is_ok_and(|value| {
                value
                    .to_string()
                    .split(',')
                    .any(|allowed| allowed.trim() == origin)
            })
    })
}
