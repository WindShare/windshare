mod edge;
mod public_directory;
mod public_dns;
mod reconcile;
mod route;
use super::{
    NetworkAuthority, WorkerEnvironment, failure,
    routes::{self, Context},
    storage::Database,
};
use crate::{
    Authority, Environment, Fault,
    fleet::{
        publication::{KV_KEY, Publication, Publisher, Release},
        topology::Spec,
    },
};
use anyhow::Result;
pub(super) use edge::{handle_edge, handles_edge};
pub(super) use public_directory::serve as public_directory;
use reconcile::{DirectoryMirror, ReconcileMode, ReconcileOutcome};
use route::PostCommitReconcile;
pub(super) use route::Route;
use serde::Serialize;
use serde_json::{Value, json};
use worker::{Env, Method, Request, RequestInit, Response, ScheduleContext, ScheduledEvent, event};
const AUTHORITY_OBJECT: &str = "network-authority-v1";
// Cloudflare Workers KV enforces a 60 s minimum cache_ttl on reads.
const KV_AUDIT_CACHE_SECONDS: u64 = 60;
const EXPIRY_ALERT_SECONDS: u64 = 14 * 86400;
const MAX_JS_DATE_MS: u64 = 8_640_000_000_000_000;
// Clamp sub-second alarm re-arms so early alarm jitter past a cooldown boundary
// does not hot-spin while waiting for the remaining few milliseconds.
const MIN_ALARM_DELAY_MS: u64 = 500;

#[expect(
    clippy::cast_precision_loss,
    reason = "JavaScript Date's supported integer range is below 2^53"
)]
fn alarm_time(at_ms: u64) -> Result<worker::ScheduledTime> {
    anyhow::ensure!(
        at_ms <= MAX_JS_DATE_MS,
        "alarm timestamp exceeds JavaScript Date range"
    );
    Ok(worker::ScheduledTime::new(js_sys::Date::new(
        &wasm_bindgen::JsValue::from_f64(at_ms as f64),
    )))
}

struct KvMirror(worker::kv::KvStore);

impl DirectoryMirror for KvMirror {
    async fn read(&self) -> Result<Option<String>> {
        self.0
            .get(KV_KEY)
            .cache_ttl(KV_AUDIT_CACHE_SECONDS)
            .text()
            .await
            .map_err(|error| anyhow::anyhow!("{error}"))
    }

    async fn write(&self, body: &str) -> Result<()> {
        self.0
            .put(KV_KEY, body)
            .map_err(|error| anyhow::anyhow!("{error}"))?
            .execute()
            .await
            .map_err(|error| anyhow::anyhow!("{error}"))
    }
}

// Keep TEST_DIRECTORY_RELEASE overrides out of release Worker deployments so
// production binaries stay pinned to the compiled offline root keys.
#[cfg(all(feature = "test-release", not(debug_assertions)))]
compile_error!("`test-release` feature must not be enabled in release Worker builds");

fn release(#[allow(unused_variables)] env: &Env) -> Result<Release> {
    #[cfg(feature = "test-release")]
    if let Ok(value) = env.var("TEST_DIRECTORY_RELEASE") {
        return Ok(serde_json::from_str(&value.to_string())?);
    }
    Release::bundled()
}

fn directory(store: &mut Database, #[allow(unused_variables)] env: &Env) -> Result<Publication> {
    if let Some(head) = Publication::load(store)? {
        return Ok(head);
    }
    #[cfg(feature = "test-release")]
    if let Ok(value) = env.var("TEST_DIRECTORY_RELEASE") {
        return Publication::bootstrap(store, &serde_json::from_str(&value.to_string())?, &[]);
    }
    Publication::import_bundled(store)
}
fn signer(env: &Env) -> Result<Publisher> {
    Publisher::new(
        &release(env)?,
        serde_json::from_str(&env.var("DIRECTORY_CERTIFICATE")?.to_string())?,
        &env.secret("DIRECTORY_SIGNING_SEED")?.to_string(),
    )
}
type Service = Authority<Database, WorkerEnvironment>;
pub(super) fn console(authority: &mut Service, env: &Env) -> Result<Value> {
    let publication = directory(&mut authority.store, env)?;
    let revision = publication.revision()?;
    let now = authority.environment.now();
    let mut value = serde_json::to_value(&revision)?;
    value["kv_sync_pending"] = json!(reconcile::sync_pending(&authority.store, &revision.digest)?);
    let ticket_expiry = authority.signing.generation.expires_at;
    let alert_time = now + EXPIRY_ALERT_SECONDS;
    value["expiring_certificates"] = json!({
        "ticket": (ticket_expiry <= alert_time).then_some(ticket_expiry),
        "directory": (revision.certificate_expires <= i64::try_from(alert_time)?).then_some(revision.certificate_expires)
    });
    if let Ok(origin) = env.var("API_ORIGIN") {
        value["setup"] = json!({"url":origin.to_string(),"issuer":env.var("AUTHORITY_ROOT")?.to_string(),"certificate":authority.signing.certificate});
    }
    Ok(value)
}
async fn internal(
    env: &Env,
    route: Route,
    auth: (Option<&str>, Option<&str>),
    body: &impl Serialize,
) -> worker::Result<Response> {
    let payload = serde_json::to_vec(body).map_err(failure)?;
    let url = format!("https://internal{}", route.path());
    let body = js_sys::Uint8Array::from(payload.as_slice());
    let mut request = Request::new_with_init(
        &url,
        RequestInit::new()
            .with_method(Method::Post)
            .with_body(Some(body.into())),
    )?;
    let headers = request.headers_mut()?;
    headers.set("Content-Type", "application/json")?;
    headers.set("X-Authority-Source", "fleet")?;
    if let Some(cookie) = auth.0 {
        headers.set("Cookie", cookie)?;
    }
    if let Some(csrf) = auth.1 {
        headers.set("X-CSRF-Token", csrf)?;
    }
    env.durable_object("AUTHORITY")?
        .id_from_name(AUTHORITY_OBJECT)?
        .get_stub()?
        .fetch_with_request(request)
        .await
}

#[event(scheduled)]
pub async fn scheduled(_event: ScheduledEvent, env: Env, context: ScheduleContext) {
    super::telemetry::install(&env);
    context.wait_until(async move {
        if let Err(error) = scheduled_tick(&env).await {
            tracing::error!(event="directory_tick_failed",%error);
        }
    });
}
async fn scheduled_tick(env: &Env) -> worker::Result<()> {
    let response = internal(env, Route::Tick, (None, None), &json!({})).await?;
    if response.status_code() != 200 {
        return Err(failure("directory tick unavailable"));
    }
    Ok(())
}
impl NetworkAuthority {
    pub(super) async fn fleet(
        &self,
        route: Route,
        req: &Request,
        bytes: &[u8],
    ) -> worker::Result<Response> {
        if req.method() != Method::Post {
            return Response::error("not found", 404);
        }
        let ctx = Context::new(req, &self.env)?;
        let bytes = bytes.to_vec();
        // A rejected fleet command rolls back the whole transaction, including
        // bootstrap and projection writes. Network reads and KV writes run outside it.
        let result = self.transact(false, move |service, env| {
            dispatch(service, env, route, &ctx, &bytes)
        });
        // A failed renewal transaction cannot strand the last committed head in SQLite.
        let reconcile_mode = match route.reconcile_policy() {
            PostCommitReconcile::AlwaysAudit => Some(ReconcileMode::AuditKv),
            PostCommitReconcile::OnCommitPending if result.is_ok() => {
                Some(ReconcileMode::PendingOnly)
            }
            PostCommitReconcile::OnCommitPending | PostCommitReconcile::None => None,
        };
        if let Some(mode) = reconcile_mode {
            // The SQLite mutation is already committed; post-commit KV/alarm errors
            // are logged inside reconcile_and_schedule and recovered by alarm or cron.
            let _ = self.reconcile_and_schedule(mode).await;
        }
        match result {
            Ok(value) => Response::from_json(&value),
            Err(error) => error.downcast_ref::<Fault>().map_or_else(
                || {
                    tracing::error!(event="fleet_command_failed",%error);
                    Response::error("fleet temporarily unavailable", 503)
                },
                |fault| routes::fault(fault).map_err(failure),
            ),
        }
    }

    pub(super) async fn reconcile_alarm(&self) -> Result<()> {
        match self
            .reconcile_and_schedule(ReconcileMode::PendingOnly)
            .await
        {
            Ok(outcome) => {
                tracing::info!(event = "directory_kv_alarm_handled", ?outcome);
                Ok(())
            }
            Err(error) => {
                tracing::warn!(event = "directory_kv_alarm_failed", %error);
                Err(error)
            }
        }
    }

    async fn reconcile_and_schedule(&self, mode: ReconcileMode) -> Result<ReconcileOutcome> {
        let _writer = self.publishing.lock().await;
        let outcome = self.reconcile_locked(mode).await.inspect_err(|error| {
            tracing::warn!(event = "directory_kv_reconcile_failed", ?mode, %error);
        })?;
        self.sync_kv_alarm(outcome).await.inspect_err(|error| {
            tracing::warn!(event = "directory_kv_alarm_update_failed", ?outcome, %error);
        })?;
        Ok(outcome)
    }

    async fn reconcile_locked(&self, mode: ReconcileMode) -> Result<ReconcileOutcome> {
        let mut db = self.database()?;
        let mirror = KvMirror(
            self.env
                .kv("DIRECTORY")
                .map_err(|error| anyhow::anyhow!("{error}"))?,
        );
        reconcile::reconcile(&mut db, &mirror, mode, || worker::Date::now().as_millis()).await
    }

    async fn sync_kv_alarm(&self, outcome: ReconcileOutcome) -> Result<()> {
        let storage = self.state.storage();
        if let Some(delay_ms) = outcome.retry_after_ms() {
            let now_ms = worker::Date::now().as_millis();
            let deadline = now_ms.saturating_add(delay_ms.max(MIN_ALARM_DELAY_MS));
            if self
                .kv_alarm_deadline
                .get()
                .is_some_and(|at| at <= deadline)
            {
                return Ok(());
            }
            let existing = storage
                .get_alarm()
                .await
                .map_err(|error| anyhow::anyhow!("{error}"))?
                .and_then(|at| u64::try_from(at).ok());
            if let Some(at) = existing
                && at <= deadline
            {
                self.kv_alarm_deadline.set(Some(at));
                return Ok(());
            }
            storage
                .set_alarm(alarm_time(deadline)?)
                .await
                .map_err(|error| anyhow::anyhow!("{error}"))?;
            self.kv_alarm_deadline.set(Some(deadline));
            tracing::info!(
                event = "directory_kv_alarm_scheduled",
                ?outcome,
                delay_ms = deadline.saturating_sub(now_ms),
                deadline_ms = deadline
            );
            return Ok(());
        }
        if self.kv_alarm_deadline.get().is_some() {
            // KV is already synced (or rapid retries are intentionally exhausted);
            // clearing a redundant alarm is best-effort and must not fail the alarm handler.
            if let Err(error) = storage.delete_alarm().await {
                tracing::warn!(event = "directory_kv_alarm_clear_failed", ?outcome, %error);
            }
            // Invalidate the cached deadline either way so the next required arm
            // checks authoritative alarm storage instead of trusting a stale cache.
            self.kv_alarm_deadline.set(None);
        }
        Ok(())
    }
}

fn dispatch(
    service: &mut Service,
    env: &Env,
    route: Route,
    ctx: &Context,
    bytes: &[u8],
) -> Result<Value> {
    if route != Route::Tick {
        routes::verify_admin_csrf(&ctx.session, &ctx.csrf)?;
        service.require_admin(&ctx.session)?;
    }
    match route {
        Route::Prepare => {}
        Route::Enroll => {
            directory(&mut service.store, env)?;
            let spec: Spec = routes::parse(bytes)?;
            let node = service.enroll(&ctx.session, spec, &signer(env)?)?;
            return Ok(json!(service.node_details(node)?));
        }
        Route::Manage => {
            directory(&mut service.store, env)?;
            service.manage(&ctx.session, &routes::parse(bytes)?, &signer(env)?)?;
        }
        Route::Tick => {
            let mut head = directory(&mut service.store, env)?;
            return tick(service, env, &mut head);
        }
    }
    Ok(json!({}))
}

fn tick(service: &mut Service, env: &Env, head: &mut Publication) -> Result<Value> {
    let now = service.environment.now();
    let expiry = head.revision()?.certificate_expires;
    if expiry <= i64::try_from(now + EXPIRY_ALERT_SECONDS)?
        || service.signing.generation.expires_at <= now + EXPIRY_ALERT_SECONDS
    {
        tracing::warn!(
            event = "network_certificate_expiring",
            directory_expires = expiry,
            ticket_expires = service.signing.generation.expires_at
        );
    }
    match signer(env) {
        Ok(publisher) => {
            if head.renew(&publisher, now)? {
                head.save(&mut service.store)?;
            }
        }
        Err(error) => tracing::error!(event="directory_renewal_failed", %error),
    }
    Ok(json!({}))
}
