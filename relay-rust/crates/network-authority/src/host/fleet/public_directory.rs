use crate::fleet::{
    publication::{KV_KEY, Release, digest},
    signature::Envelope,
};
use anyhow::{Context as _, Result};
use chrono::Duration;
use std::{cell::RefCell, rc::Rc, sync::OnceLock};
use worker::{Env, Method, Response};

const METHODS: &str = "GET, HEAD, OPTIONS";
const CACHE_SECONDS: u64 = 60;

struct VerifiedDirectory {
    body: String,
    start: i64,
    until: i64,
    version: (u64, u64),
    digest: String,
}
impl VerifiedDirectory {
    fn verified(body: String) -> Result<Self> {
        static TRUST: OnceLock<Release> = OnceLock::new();
        if TRUST.get().is_none() {
            let _ = TRUST.set(Release::bundled()?);
        }
        let release = TRUST.get().context("missing release trust")?;
        let envelope = Envelope::decode(body.as_bytes())?;
        let (payload, grant) = envelope.verified_parts(&release.network, &release.roots, false)?;
        Ok(Self {
            body,
            start: payload.issued_at.timestamp() - Duration::minutes(5).num_seconds(),
            until: (payload.expires_at + Duration::days(7))
                .min(grant.expires_at)
                .timestamp(),
            version: (payload.generation, payload.serial),
            digest: digest(&envelope)?,
        })
    }
    const fn usable(&self, now: i64) -> bool {
        self.start <= now && now < self.until
    }
}

thread_local! {
    static LATEST: RefCell<Option<Rc<VerifiedDirectory>>> = const { RefCell::new(None) };
}

fn select(body: Option<String>, now: i64) -> Result<Option<Rc<VerifiedDirectory>>> {
    LATEST.with_borrow_mut(|entry| {
        if entry.is_none() {
            let release = Release::bundled()?;
            *entry = Some(Rc::new(VerifiedDirectory::verified(
                serde_json::to_string(&release.snapshot)?,
            )?));
        }
        if let Some(body) = body
            && entry.as_ref().is_none_or(|copy| copy.body != body)
        {
            let next = VerifiedDirectory::verified(body)?;
            let newer = entry
                .as_ref()
                .is_none_or(|copy| next.version > copy.version);
            if let Some(copy) = &entry
                && next.version == copy.version
            {
                anyhow::ensure!(next.digest == copy.digest, "directory equivocation");
            }
            if newer && next.usable(now) {
                *entry = Some(Rc::new(next));
            }
        }
        Ok(entry.as_ref().filter(|copy| copy.usable(now)).cloned())
    })
}

async fn response(method: Method, env: &Env) -> worker::Result<Response> {
    if method == Method::Options {
        return Ok(Response::empty()?.with_status(204));
    }
    if !matches!(method, Method::Get | Method::Head) {
        return Ok(Response::empty()?.with_status(405));
    }
    let now = i64::try_from(worker::Date::now().as_millis() / 1000).map_err(super::failure)?;
    let lookup = async {
        env.kv("DIRECTORY")?
            .get(KV_KEY)
            .cache_ttl(CACHE_SECONDS)
            .text()
            .await
            .map_err(worker::Error::from)
    }
    .await;
    let body = lookup.unwrap_or_else(|error| {
        tracing::warn!(event="directory_kv_read_failed", %error);
        None
    });
    let selected = select(body, now).or_else(|error| {
        tracing::warn!(event="directory_copy_rejected", %error);
        select(None, now)
    });
    let Some(copy) = selected.unwrap_or_default() else {
        let mut response = Response::empty()?.with_status(503);
        response.headers_mut().set("Retry-After", "30")?;
        return Ok(response);
    };
    let mut response = if method == Method::Head {
        Response::empty()?
    } else {
        Response::ok(&copy.body)?
    };
    response
        .headers_mut()
        .set("Content-Type", "application/json")?;
    let ttl = copy.until.saturating_sub(now).min(60);
    response
        .headers_mut()
        .set("Cache-Control", &format!("public, max-age={ttl}"))?;
    Ok(response)
}

pub(in crate::host) async fn serve(method: Method, env: &Env) -> worker::Result<Response> {
    let mut response = response(method, env).await?;
    if response.status_code() != 200 {
        response.headers_mut().set("Cache-Control", "no-store")?;
    }
    let headers = response.headers_mut();
    headers.set("Access-Control-Allow-Origin", "*")?;
    headers.set("Access-Control-Allow-Methods", METHODS)?;
    headers.set("Access-Control-Allow-Headers", "Accept, Content-Type")?;
    headers.set("Allow", METHODS)?;
    headers.set("X-Content-Type-Options", "nosniff")?;
    headers.set("Referrer-Policy", "no-referrer")?;
    Ok(response)
}
