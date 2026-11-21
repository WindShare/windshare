use super::{DIRECTORY_PATH, internal, release};
use crate::fleet::{
    descriptor::{self, MAX_DESCRIPTOR},
    publication::KV_KEY,
    signature::{Envelope, MAX_BYTES},
    topology::{Role, Spec, Status, public_origin},
};
use crate::{
    Fault, Zone,
    host::{failure, read_body, routes},
};
use futures_util::StreamExt;
use serde::{Deserialize, Serialize};
use serde_json::json;
use worker::{
    CacheMode, Env, Fetch, Method, Request, RequestInit, RequestRedirect, Response, Result,
};

const REQUEST_TIMEOUT_MS: u32 = 5000;
pub(in crate::host) fn handles_edge(req: &Request) -> bool {
    req.method() == Method::Post
        && matches!(
            req.path().as_str(),
            "/v1/admin/nodes/discover" | "/v1/admin/nodes" | "/v1/admin/nodes/promote"
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
#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(super) struct Promote {
    pub node_key: String,
    pub status: Status,
}

pub(super) async fn fetch_public(url: &str, limit: usize, accept: &str) -> anyhow::Result<Vec<u8>> {
    let mut req = Request::new_with_init(
        url,
        RequestInit::new()
            .with_method(Method::Get)
            .with_redirect(RequestRedirect::Manual)
            .with_cache(CacheMode::NoStore),
    )?;
    req.headers_mut()?.set("Accept", accept)?;
    let signal = web_sys::AbortSignal::timeout_with_u32(REQUEST_TIMEOUT_MS);
    let mut response = Fetch::Request(req).send_with_signal(&signal.into()).await?;
    anyhow::ensure!(response.status_code() == 200, "remote endpoint unavailable");
    let mut stream = response.stream()?;
    let mut bytes = Vec::new();
    while let Some(chunk) = stream.next().await {
        let chunk = chunk?;
        anyhow::ensure!(
            bytes.len() + chunk.len() <= limit,
            "remote response too large"
        );
        bytes.extend_from_slice(&chunk);
    }
    Ok(bytes)
}

pub(super) async fn mirrors_match(expected: &Envelope) -> bool {
    let Ok(release) = release() else { return false };
    let check = async |origin: &str| {
        let Ok(bytes) = fetch_public(
            &format!("{origin}{DIRECTORY_PATH}"),
            MAX_BYTES,
            "application/json",
        )
        .await
        else {
            return false;
        };
        serde_json::from_slice::<Envelope>(&bytes).is_ok_and(|actual| actual == *expected)
    };
    let (primary, backup) =
        futures_util::future::join(check(&release.link_host), check(&release.backup_entry)).await;
    tracing::info!(
        event = "directory_mirrors_checked",
        revision = %crate::fleet::publication::digest(expected).unwrap_or_default(),
        primary,
        backup
    );
    primary && backup
}

pub(in crate::host) async fn handle_edge(mut req: Request, env: &Env) -> Result<Response> {
    let cookie = req.headers().get("Cookie")?;
    let csrf = req.headers().get("X-CSRF-Token")?;
    let auth = (cookie.as_deref(), csrf.as_deref());
    let mut verified = internal(env, "prepare", auth, &json!({})).await?;
    if verified.status_code() != 200 {
        return Ok(verified);
    }
    let Some(bytes) = read_body(&mut req).await? else {
        return Response::error("request too large", 413);
    };
    if req.path() == "/v1/admin/nodes/promote" {
        let Ok(command) = serde_json::from_slice::<Promote>(&bytes) else {
            return invalid();
        };
        let expected = verified.json::<Envelope>().await?;
        if !mirrors_match(&expected).await {
            return routes::fault(&Fault::Denied {
                reason: "mirror_lag".into(),
            })
            .map_err(failure);
        }
        return internal(
            env,
            "promote",
            auth,
            &json!({"command":command,"observed":expected}),
        )
        .await;
    }
    let (address, command) = if req.path().ends_with("/discover") {
        let Ok(input) = serde_json::from_slice::<Discover>(&bytes) else {
            return invalid();
        };
        (input.url, None)
    } else {
        let Ok(input) = serde_json::from_slice::<Enroll>(&bytes) else {
            return invalid();
        };
        (input.url.clone(), Some(input))
    };
    let Ok(origin) = public_origin(&address) else {
        return invalid();
    };
    if super::public_dns::validate(&origin).await.is_err() {
        return invalid();
    }
    let Ok(bytes) = fetch_public(
        &format!("{origin}/.well-known/windshare"),
        MAX_DESCRIPTOR,
        "application/json",
    )
    .await
    else {
        return invalid();
    };
    let Ok(discovery) =
        descriptor::inspect(&origin, &bytes, worker::Date::now().as_millis() / 1000)
    else {
        return invalid();
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
        return invalid();
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
    internal(env, "stage", auth, &spec).await
}
fn invalid() -> Result<Response> {
    routes::fault(&Fault::Invalid).map_err(failure)
}

pub(in crate::host) async fn public_directory(env: &Env) -> Result<Response> {
    let body = env
        .kv("DIRECTORY")?
        .get(KV_KEY)
        .cache_ttl(60)
        .text()
        .await?;
    let Some(body) = body else {
        return Response::error("directory unavailable", 503);
    };
    let mut response = Response::ok(body)?;
    response
        .headers_mut()
        .set("Content-Type", "application/json")?;
    response
        .headers_mut()
        .set("Access-Control-Allow-Origin", "*")?;
    response
        .headers_mut()
        .set("Cache-Control", "public, max-age=60")?;
    Ok(response)
}
