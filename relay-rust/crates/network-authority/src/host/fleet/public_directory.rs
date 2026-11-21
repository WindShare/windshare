use crate::fleet::{
    publication::{Release, digest},
    signature::{Envelope, MAX_BYTES},
};
use anyhow::Result;
use chrono::Duration;
use std::rc::Rc;
#[cfg(target_arch = "wasm32")]
use {
    crate::fleet::publication::KV_KEY,
    anyhow::Context as _,
    std::{cell::RefCell, sync::OnceLock},
    worker::{Env, Method, Response},
};

#[cfg(target_arch = "wasm32")]
const METHODS: &str = "GET, HEAD, OPTIONS";
#[cfg(target_arch = "wasm32")]
const CACHE_SECONDS: u32 = 60;
const CLOCK_SKEW: Duration = Duration::minutes(5);
const GRACE_PERIOD: Duration = Duration::days(7);

pub struct VerifiedDirectory {
    pub body: String,
    start: i64,
    until: i64,
    version: (u64, u64),
    digest: String,
}

impl VerifiedDirectory {
    pub const fn usable(&self, now: i64) -> bool {
        self.start <= now && now < self.until
    }
}

pub struct VerifiedMetadata {
    start: i64,
    until: i64,
    version: (u64, u64),
    digest: String,
}

impl VerifiedMetadata {
    pub fn verify(body: &str, release: &Release) -> Result<Self> {
        let envelope = Envelope::decode(body.as_bytes())?;
        let (payload, grant) = envelope.verified_parts(&release.network, &release.roots, false)?;
        Ok(Self {
            start: (payload.issued_at - CLOCK_SKEW).timestamp(),
            until: (payload.expires_at + GRACE_PERIOD)
                .min(grant.expires_at)
                .timestamp(),
            version: (payload.generation, payload.serial),
            digest: digest(&envelope)?,
        })
    }

    fn with_body(self, body: String) -> VerifiedDirectory {
        VerifiedDirectory {
            body,
            start: self.start,
            until: self.until,
            version: self.version,
            digest: self.digest,
        }
    }
}

enum EvaluatedKv {
    /// Verified candidate retained so stale copies skip repeat Ed25519 checks while
    /// future-dated copies can still be promoted once `now` reaches `start`.
    Verified(Rc<VerifiedDirectory>),
    /// Rejected copy within wire bounds; exact matching suppresses repeat verification
    /// and duplicate warnings until KV actually changes.
    Rejected(String),
    /// Rejected copy above `MAX_BYTES`; tracking byte length avoids pinning large blobs
    /// inside a Workers isolate.
    Oversized(usize),
}

impl EvaluatedKv {
    fn matches(&self, raw: &str) -> bool {
        match self {
            Self::Verified(copy) => copy.body == raw,
            Self::Rejected(body) => body == raw,
            Self::Oversized(len) => raw.len() > MAX_BYTES && raw.len() == *len,
        }
    }

    fn rejected(body: String) -> Self {
        let len = body.len();
        if len > MAX_BYTES {
            Self::Oversized(len)
        } else {
            Self::Rejected(body)
        }
    }
}

#[derive(Default)]
pub struct DirectoryCache {
    latest: Option<Rc<VerifiedDirectory>>,
    last_kv: Option<EvaluatedKv>,
}

impl DirectoryCache {
    pub const fn new() -> Self {
        Self {
            latest: None,
            last_kv: None,
        }
    }

    pub fn select(
        &mut self,
        release: &Release,
        body: Option<String>,
        now: i64,
    ) -> Result<Option<Rc<VerifiedDirectory>>> {
        self.select_with(release, body, now, &mut VerifiedMetadata::verify)
    }

    pub fn select_with(
        &mut self,
        release: &Release,
        body: Option<String>,
        now: i64,
        verify: &mut impl FnMut(&str, &Release) -> Result<VerifiedMetadata>,
    ) -> Result<Option<Rc<VerifiedDirectory>>> {
        let latest = self.ensure_bundled(release, verify)?;
        let Some(body) = body else {
            self.last_kv = None;
            return Ok(latest.usable(now).then(|| Rc::clone(&latest)));
        };
        if latest.body == body {
            self.last_kv = None;
            return Ok(latest.usable(now).then(|| Rc::clone(&latest)));
        }
        if let Some(candidate) = self.evaluate_kv(release, &latest, body, verify)
            && candidate.version > latest.version
            && candidate.usable(now)
        {
            self.latest = Some(Rc::clone(&candidate));
            self.last_kv = None;
            return Ok(Some(candidate));
        }
        Ok(latest.usable(now).then(|| Rc::clone(&latest)))
    }

    fn ensure_bundled(
        &mut self,
        release: &Release,
        verify: &mut impl FnMut(&str, &Release) -> Result<VerifiedMetadata>,
    ) -> Result<Rc<VerifiedDirectory>> {
        if let Some(latest) = &self.latest {
            return Ok(Rc::clone(latest));
        }
        let body = serde_json::to_string(&release.snapshot)?;
        let copy = Rc::new(verify(&body, release)?.with_body(body));
        self.latest = Some(Rc::clone(&copy));
        Ok(copy)
    }

    fn evaluate_kv(
        &mut self,
        release: &Release,
        latest: &VerifiedDirectory,
        body: String,
        verify: &mut impl FnMut(&str, &Release) -> Result<VerifiedMetadata>,
    ) -> Option<Rc<VerifiedDirectory>> {
        if let Some(cached) = &self.last_kv
            && cached.matches(&body)
        {
            return match cached {
                EvaluatedKv::Verified(copy) => Some(Rc::clone(copy)),
                EvaluatedKv::Rejected(_) | EvaluatedKv::Oversized(_) => None,
            };
        }
        let verified = verify(&body, release).and_then(|next| {
            if next.version == latest.version {
                anyhow::ensure!(next.digest == latest.digest, "directory equivocation");
            }
            Ok(next)
        });
        match verified {
            Ok(next) => {
                let copy = Rc::new(next.with_body(body));
                self.last_kv = Some(EvaluatedKv::Verified(Rc::clone(&copy)));
                Some(copy)
            }
            Err(error) => {
                tracing::warn!(event = "directory_copy_rejected", bytes = body.len(), %error);
                self.last_kv = Some(EvaluatedKv::rejected(body));
                None
            }
        }
    }
}

#[cfg(target_arch = "wasm32")]
fn bundled_release() -> Result<&'static Release> {
    static TRUST: OnceLock<Release> = OnceLock::new();
    if let Some(release) = TRUST.get() {
        return Ok(release);
    }
    let _ = TRUST.set(Release::bundled()?);
    TRUST.get().context("missing release trust")
}

#[cfg(target_arch = "wasm32")]
thread_local! {
    static CACHE: RefCell<DirectoryCache> = const { RefCell::new(DirectoryCache::new()) };
}

#[cfg(target_arch = "wasm32")]
fn select(body: Option<String>, now: i64) -> Result<Option<Rc<VerifiedDirectory>>> {
    let release = bundled_release()?;
    CACHE.with_borrow_mut(|cache| cache.select(release, body, now))
}

#[cfg(target_arch = "wasm32")]
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
            .cache_ttl(u64::from(CACHE_SECONDS))
            .text()
            .await
            .map_err(worker::Error::from)
    }
    .await;
    let body = lookup.unwrap_or_else(|error| {
        tracing::warn!(event = "directory_kv_read_failed", %error);
        None
    });
    let selected = select(body, now).unwrap_or_else(|error| {
        tracing::error!(event = "directory_bundled_invalid", %error);
        None
    });
    let Some(copy) = selected else {
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
    let ttl = copy.until.saturating_sub(now).min(i64::from(CACHE_SECONDS));
    response
        .headers_mut()
        .set("Cache-Control", &format!("public, max-age={ttl}"))?;
    Ok(response)
}

#[cfg(target_arch = "wasm32")]
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
