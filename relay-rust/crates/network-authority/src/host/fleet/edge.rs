use super::{Route, internal, release};
use crate::fleet::{
    descriptor::{self, MAX_DESCRIPTOR},
    topology::{Role, Spec, public_origin},
};
use crate::{
    DiscoveryFailure, Fault, Zone,
    host::{failure, read_body, routes},
};
use futures_util::StreamExt;
use serde::Deserialize;
use serde_json::json;
use std::fmt;
use worker::{
    CacheMode, Env, Fetch, Method, Request, RequestInit, RequestRedirect, Response, Result,
};

const REQUEST_TIMEOUT_MS: u32 = 5000;
const HTTP_OK: u16 = 200;
// Match the WHATWG Fetch redirect status set rather than treating 300/304 as redirects.
const REDIRECT_STATUSES: [u16; 5] = [301, 302, 303, 307, 308];

#[derive(Debug)]
pub(super) enum FetchError {
    Transport(String),
    Redirect(u16),
    HttpStatus(u16),
    TooLarge,
}
impl fmt::Display for FetchError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Transport(detail) => f.write_str(detail),
            Self::Redirect(status) => write!(f, "remote redirect HTTP {status}"),
            Self::HttpStatus(status) => write!(f, "remote HTTP {status}"),
            Self::TooLarge => f.write_str("remote response too large"),
        }
    }
}
impl std::error::Error for FetchError {}

#[derive(Debug)]
pub(super) struct DiscoveryRejection {
    pub(super) failure: DiscoveryFailure,
    pub(super) detail: String,
}
impl DiscoveryRejection {
    pub(super) fn new(failure: DiscoveryFailure, detail: impl Into<String>) -> Self {
        Self {
            failure,
            detail: detail.into(),
        }
    }
}

pub(in crate::host) fn handles_edge(req: &Request) -> bool {
    req.method() == Method::Post
        && matches!(
            req.path().as_str(),
            "/v1/admin/nodes/discover" | "/v1/admin/nodes"
        )
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Discover {
    url: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Enroll {
    url: String,
    node_key: String,
    alias: String,
    zone: Zone,
    region: String,
    provider: String,
    roles: Vec<Role>,
    capacity_mbps: u32,
}

pub(super) async fn fetch_public(
    url: &str,
    limit: usize,
    accept: &str,
) -> std::result::Result<Vec<u8>, FetchError> {
    let mut req = Request::new_with_init(
        url,
        RequestInit::new()
            .with_method(Method::Get)
            .with_redirect(RequestRedirect::Manual)
            .with_cache(CacheMode::NoStore),
    )
    .map_err(|error| FetchError::Transport(error.to_string()))?;
    req.headers_mut()
        .and_then(|headers| headers.set("Accept", accept))
        .map_err(|error| FetchError::Transport(error.to_string()))?;
    let signal = web_sys::AbortSignal::timeout_with_u32(REQUEST_TIMEOUT_MS);
    let mut response = Fetch::Request(req)
        .send_with_signal(&signal.into())
        .await
        .map_err(|error| FetchError::Transport(error.to_string()))?;
    let status = response.status_code();
    if REDIRECT_STATUSES.contains(&status) {
        return Err(FetchError::Redirect(status));
    }
    if status != HTTP_OK {
        return Err(FetchError::HttpStatus(status));
    }
    let mut stream = response
        .stream()
        .map_err(|error| FetchError::Transport(error.to_string()))?;
    let mut bytes = Vec::new();
    while let Some(chunk) = stream.next().await {
        let chunk = chunk.map_err(|error| FetchError::Transport(error.to_string()))?;
        if bytes.len() + chunk.len() > limit {
            return Err(FetchError::TooLarge);
        }
        bytes.extend_from_slice(&chunk);
    }
    Ok(bytes)
}

pub(in crate::host) async fn handle_edge(mut req: Request, env: &Env) -> Result<Response> {
    let cookie = req.headers().get("Cookie")?;
    let csrf = req.headers().get("X-CSRF-Token")?;
    let auth = (cookie.as_deref(), csrf.as_deref());
    let verified = internal(env, Route::Prepare, auth, &json!({})).await?;
    if verified.status_code() != HTTP_OK {
        return Ok(verified);
    }
    let Some(bytes) = read_body(&mut req).await? else {
        return Response::error("request too large", 413);
    };
    handle_discover_or_enroll(&req, &bytes, env, auth).await
}

async fn handle_discover_or_enroll(
    req: &Request,
    bytes: &[u8],
    env: &Env,
    auth: (Option<&str>, Option<&str>),
) -> Result<Response> {
    let (address, command) = if req.path().ends_with("/discover") {
        let Ok(input) = serde_json::from_slice::<Discover>(bytes) else {
            return invalid();
        };
        (input.url, None)
    } else {
        let Ok(input) = serde_json::from_slice::<Enroll>(bytes) else {
            return invalid();
        };
        (input.url.clone(), Some(input))
    };
    let Ok(origin) = public_origin(&address) else {
        tracing::warn!(
            event = "node_discovery_rejected",
            reason = DiscoveryFailure::InvalidUrl.reason()
        );
        return discovery_fault(DiscoveryFailure::InvalidUrl);
    };
    let discovery = match discover_node(&origin, req, env).await? {
        Ok(node) => node,
        Err(rejection) => {
            tracing::warn!(
                event = "node_discovery_rejected",
                %origin,
                reason = rejection.failure.reason(),
                error = %rejection.detail
            );
            return discovery_fault(rejection.failure);
        }
    };
    let Some(command) = command else {
        return Response::from_json(&discovery);
    };
    if command.node_key != discovery.node_key
        || !command
            .roles
            .iter()
            .all(|role| discovery.roles.contains(role))
    {
        tracing::warn!(
            event = "node_enrollment_mismatch",
            %origin,
            requested_node_key = %command.node_key,
            discovered_node_key = %discovery.node_key
        );
        return routes::fault(&Fault::Conflict {
            reason: "node_settings_changed".into(),
        })
        .map_err(failure);
    }
    let spec = Spec {
        node_key: discovery.node_key,
        relay: origin,
        stun: if command.roles.contains(&Role::Stun) {
            discovery.stun
        } else {
            Vec::new()
        },
        roles: command.roles,
        zone: command.zone,
        region: command.region,
        provider: command.provider,
        alias: command.alias,
        operator: "official".into(),
        capacity_mbps: command.capacity_mbps,
    };
    internal(env, Route::Enroll, auth, &spec).await
}

fn invalid() -> Result<Response> {
    routes::fault(&Fault::Invalid).map_err(failure)
}

fn discovery_fault(cause: DiscoveryFailure) -> Result<Response> {
    routes::fault(&Fault::node_discovery(cause)).map_err(failure)
}

async fn discover_node(
    origin: &str,
    req: &Request,
    env: &Env,
) -> Result<std::result::Result<descriptor::Discovery, DiscoveryRejection>> {
    let host = url::Url::parse(origin).map_err(failure)?;
    let release = release(env).map_err(failure)?;
    let api = env.var("API_ORIGIN")?.to_string();
    let own = req.url()?.origin().ascii_serialization();
    let direct = env.var("AUTHORITY_ORIGIN").ok().map(|v| v.to_string());
    let issuer = env.var("AUTHORITY_ROOT")?.to_string();
    let origins: Vec<_> = [Some(api.as_str()), direct.as_deref(), Some(own.as_str())]
        .into_iter()
        .flatten()
        .collect();
    for protected in [release.link_host.as_str(), release.backup_entry.as_str()]
        .into_iter()
        .chain(origins.iter().copied())
    {
        let url = url::Url::parse(protected).map_err(failure)?;
        if host.host() == url.host() {
            return Ok(Err(DiscoveryRejection::new(
                DiscoveryFailure::ReservedHost,
                "reserved control or frontend hostname",
            )));
        }
    }
    if let Err(rejection) = super::public_dns::validate(origin).await {
        return Ok(Err(rejection));
    }
    let bytes = match fetch_public(
        &format!("{origin}/.well-known/windshare"),
        MAX_DESCRIPTOR,
        "application/json",
    )
    .await
    {
        Ok(bytes) => bytes,
        Err(error) => {
            let reason = match &error {
                FetchError::Transport(_) => DiscoveryFailure::Unreachable,
                FetchError::Redirect(_) => DiscoveryFailure::RedirectRefused,
                FetchError::HttpStatus(status) => DiscoveryFailure::HttpStatus { status: *status },
                FetchError::TooLarge => DiscoveryFailure::InvalidDescriptor,
            };
            return Ok(Err(DiscoveryRejection::new(reason, error.to_string())));
        }
    };
    Ok(descriptor::inspect(
        origin,
        &bytes,
        worker::Date::now().as_millis() / 1000,
        &issuer,
        &origins,
    )
    .map_err(|error| DiscoveryRejection::new(error.failure, error.detail)))
}
