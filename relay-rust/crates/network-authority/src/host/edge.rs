use serde_json::json;
use worker::{Context, Env, Method, Request, Response, Result, event};

const POLICY: &str = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'";
const IMMUTABLE_CACHE_CONTROL: &str = "public, max-age=31536000, immutable";
const PRIVATE_REVALIDATE_CACHE_CONTROL: &str = "private, no-cache";
const NO_STORE_CACHE_CONTROL: &str = "no-store";

#[derive(Clone, Copy, PartialEq, Eq)]
enum CachePolicy {
    Immutable,
    PrivateRevalidate,
    NoStore,
}

impl CachePolicy {
    const fn header(self) -> &'static str {
        match self {
            Self::Immutable => IMMUTABLE_CACHE_CONTROL,
            Self::PrivateRevalidate => PRIVATE_REVALIDATE_CACHE_CONTROL,
            Self::NoStore => NO_STORE_CACHE_CONTROL,
        }
    }
}

#[event(fetch)]
pub async fn fetch(request: Request, env: Env, _context: Context) -> Result<Response> {
    if request.path().starts_with("/internal/") {
        return Response::error("not found", 404);
    }
    let usage_report = request.method() == Method::Post && request.path() == "/v1/node/usage";
    let public_blocks =
        request.method() == Method::Get && request.path() == super::routes::PUBLIC_BLOCKS_PATH;
    let origin = request.headers().get("Origin")?;
    let own = request.url()?.origin().ascii_serialization();
    if !allowed_origin(&request.path(), origin.as_deref(), &own, &env) {
        return Response::error("origin refused", 403);
    }
    let mut cache_policy = CachePolicy::NoStore;
    let response = if request.method() == Method::Options {
        Response::empty()?.with_status(204)
    } else if request.method() == Method::Get {
        let path = request.path();
        if let Some((body, kind)) = console_asset(&path) {
            if path.starts_with("/assets/") {
                cache_policy = CachePolicy::Immutable;
            }
            asset(body, kind)?
        } else if path == "/healthz" {
            Response::from_json(&json!({"status":"ok"}))?
        } else {
            forward(request, &env).await?
        }
    } else {
        if (request.path().starts_with("/v1/admin/") || request.path().starts_with("/v1/device/"))
            && origin.is_none()
        {
            return Response::error("browser requests require an origin", 403);
        }
        if !matches!(
            request.path().as_str(),
            "/v1/node/usage" | "/v1/node/restrictions"
        ) && request
            .headers()
            .get("Content-Type")?
            .is_none_or(|value| !value.starts_with("application/json"))
        {
            return Response::error("JSON required", 415);
        }
        if super::passwords::handles(&request.method(), &request.path()) {
            super::passwords::handle(request, &env).await?
        } else {
            forward(request, &env).await?
        }
    };
    if public_blocks && matches!(response.status_code(), 200 | 304) {
        // Revalidate on each poll so normal browser caching adds no delay to blocks.
        cache_policy = CachePolicy::PrivateRevalidate;
    }
    apply_response_headers(
        response,
        &env,
        origin.as_deref(),
        cache_policy,
        usage_report,
    )
}

fn apply_response_headers(
    response: Response,
    env: &Env,
    origin: Option<&str>,
    cache_policy: CachePolicy,
    usage_report: bool,
) -> Result<Response> {
    // Durable Object fetch responses have immutable headers; preserve the body
    // and cookies while taking ownership of the edge response policy.
    let headers = response.headers().clone();
    let mut response = response.with_headers(headers);
    if usage_report && response.status_code() == 200 {
        response.headers_mut().set(
            windshare_relay_access::device_login::CAPABILITY_HEADER,
            windshare_relay_access::device_login::CAPABILITY_VERSION,
        )?;
    }
    response
        .headers_mut()
        .set("Cache-Control", cache_policy.header())?;
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
            .set("Access-Control-Allow-Origin", origin)?;
        response
            .headers_mut()
            .set("Access-Control-Allow-Credentials", "true")?;
        response.headers_mut().set(
            "Access-Control-Allow-Headers",
            "Content-Type, Authorization, X-CSRF-Token",
        )?;
        response.headers_mut().set(
            "Access-Control-Allow-Methods",
            "GET, POST, PUT, PATCH, OPTIONS",
        )?;
        response.headers_mut().set("Vary", "Origin")?;
    } else if cache_policy == CachePolicy::Immutable {
        response.headers_mut().set("Vary", "Origin")?;
    }
    Ok(response)
}
fn console_asset(path: &str) -> Option<(&'static str, &'static str)> {
    include!("../../console-dist/routes.rs")
}
fn asset(body: &str, kind: &str) -> Result<Response> {
    let mut response = Response::ok(body)?;
    response.headers_mut().set("Content-Type", kind)?;
    Ok(response)
}
pub(super) fn source_for_request(request: &Request, env: &Env) -> Result<String> {
    let peer = request.headers().get("CF-Connecting-IP")?;
    let forwarded = request.headers().get("X-Windshare-Source-Ip")?;
    let trusted = env
        .var("TRUSTED_PROXY_IPS")
        .map_or_else(|_| String::new(), |value| value.to_string());
    crate::source::source_key(peer.as_deref(), forwarded.as_deref(), &trusted)
        .map_err(super::failure)
}
async fn forward(request: Request, env: &Env) -> Result<Response> {
    let source = source_for_request(&request, env)?;
    let mut request = request.clone_mut()?;
    request.headers_mut()?.set("X-Authority-Source", &source)?;
    env.durable_object("AUTHORITY")?
        .id_from_name("network-authority-v1")?
        .get_stub()?
        .fetch_with_request(request)
        .await
}

fn allowed_origin(path: &str, origin: Option<&str>, own: &str, env: &Env) -> bool {
    let admin = path.starts_with("/v1/admin/") || path.starts_with("/v1/device/");
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
