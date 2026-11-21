mod edge;
mod maintenance;
mod public_directory;
mod public_dns;
mod reconcile;
mod route;
use super::{
    NetworkAuthority, WorkerEnvironment, failure,
    passwords::{ForwardHeaders, call_do_json_with_headers},
    routes::{self, Context},
    storage::Database,
};
use crate::{
    Authority, Environment, Fault,
    fleet::{
        publication::{self, KV_KEY, Publication, Publisher, Release},
        signature::Envelope,
        topology::Spec,
        vantage::{self, Agent},
    },
    store::{load, save},
};
use anyhow::Result;
pub(super) use edge::{handle_edge, handles_edge};
pub(super) use public_directory::serve as public_directory;
use reconcile::{DirectoryMirror, ReconcileMode, ReconcileOutcome};
pub(super) use route::Route;
use route::{AdminRoute, InternalRoute, PostCommitReconcile, VantageRoute};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use worker::{Env, Method, Request, Response, ScheduleContext, ScheduledEvent, event};
const DIRECTORY_PATH: &str = "/.well-known/windshare-network";
const MIRRORS: &str = "directory/mirrors";
const MIRROR_DEFER_HEADER: &str = "X-Directory-Mirror-Defer";
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
fn release() -> Result<Release> {
    Release::bundled()
}
fn origins() -> Result<[String; vantage::ORIGIN_COUNT]> {
    let release = release()?;
    Ok([
        release.link_host,
        release.backup_entry,
        "https://self-hosted.example".into(),
    ])
}
fn agents(env: &Env) -> Result<Vec<Agent>> {
    let agents = env.var("VANTAGE_KEYS").map_or_else(
        |_| Ok(vec![]),
        |v| serde_json::from_str::<Vec<Agent>>(&v.to_string()),
    )?;
    vantage::validate_agents(&agents)?;
    Ok(agents)
}
fn signer(env: &Env) -> Result<Publisher> {
    Publisher::new(
        &release()?,
        serde_json::from_str(&env.var("DIRECTORY_CERTIFICATE")?.to_string())?,
        &env.secret("DIRECTORY_SIGNING_SEED")?.to_string(),
    )
}
type Service = Authority<Database, WorkerEnvironment>;
pub(super) fn console(authority: &mut Service, env: &Env) -> Result<Value> {
    let publication = Publication::load_or_import(&mut authority.store)?;
    let revision = publication.revision()?;
    let now = authority.environment.now();
    let mirrors = load::<MirrorCheck>(&authority.store, MIRRORS)?;
    let enrolled = agents(env)?;
    let mainland = vantage::mainland_current(&authority.store, &enrolled, &revision.digest, now)?;
    let mut value = serde_json::to_value(&revision)?;
    value["mirrors_current"] = json!(
        mainland
            && mirrors.is_some_and(|m| m.matches
                && m.digest == revision.digest
                && now.saturating_sub(m.at) <= vantage::SILENT_SECONDS * 2)
    );
    value["silent_vantages"] = json!(vantage::silent(&authority.store, &enrolled, now)?);
    value["vantages"] = json!(crate::fleet::monitoring::reports(
        &authority.store,
        &enrolled
    )?);
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
#[derive(Deserialize, Serialize)]
struct MirrorCheck {
    digest: String,
    matches: bool,
    at: u64,
}

async fn internal(
    env: &Env,
    route: Route,
    auth: (Option<&str>, Option<&str>),
    body: &impl Serialize,
) -> worker::Result<Response> {
    call_do_json_with_headers(
        env,
        route.path(),
        "fleet",
        &ForwardHeaders {
            cookie: auth.0,
            csrf: auth.1,
            ..ForwardHeaders::default()
        },
        body,
    )
    .await
}

#[event(scheduled)]
pub async fn scheduled(_event: ScheduledEvent, env: Env, context: ScheduleContext) {
    super::telemetry::install(&env);
    context.wait_until(async move {
        if let Err(error) = maintenance(&env).await {
            tracing::error!(event="directory_maintenance_failed",%error);
        }
    });
}
async fn maintenance(env: &Env) -> worker::Result<()> {
    let mut response = internal(
        env,
        Route::Internal(InternalRoute::Tick),
        (None, None),
        &json!({}),
    )
    .await?;
    if response.status_code() != 200 {
        return Err(failure("directory maintenance unavailable"));
    }
    if let Some(reason) = response.headers().get(MIRROR_DEFER_HEADER)? {
        // Frontend mirrors cache KV reads for 60 s, so probing before KV settles
        // or while a write retry is queued would observe the previous revision.
        tracing::info!(event = "directory_mirror_check_deferred", %reason);
        return Ok(());
    }
    let expected: Envelope = response.json().await?;
    let digest = publication::digest(&expected).map_err(failure)?;
    let matches = edge::mirrors_match(&expected, &digest, None).await;
    let check = MirrorCheck {
        digest,
        matches,
        at: worker::Date::now().as_millis() / 1000,
    };
    let response = internal(
        env,
        Route::Internal(InternalRoute::Mirrors),
        (None, None),
        &check,
    )
    .await?;
    if response.status_code() != 200 {
        return Err(failure("mirror report unavailable"));
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
        // bootstrap and lease writes. Network reads and KV writes run outside it.
        let result = self.transact(
            WorkerEnvironment {
                registration_email_configured: false,
            },
            move |service, env| dispatch(service, env, route, &ctx, &bytes),
        );
        // A failed health or renewal transaction cannot strand the last committed head in SQLite.
        let reconcile_mode = match route.reconcile_policy() {
            PostCommitReconcile::AlwaysAudit => Some(ReconcileMode::AuditKv),
            PostCommitReconcile::OnCommitPending if result.is_ok() => {
                Some(ReconcileMode::PendingOnly)
            }
            PostCommitReconcile::OnCommitPending | PostCommitReconcile::None => None,
        };
        let outcome = match reconcile_mode {
            Some(mode) => self.reconcile_and_schedule(mode).await,
            None => None,
        };
        match result {
            Ok(value) => {
                let mut response = Response::from_json(&value)?;
                if route == Route::Internal(InternalRoute::Tick)
                    && let Some(reason) = outcome.and_then(ReconcileOutcome::mirror_defer_reason)
                {
                    response.headers_mut().set(MIRROR_DEFER_HEADER, reason)?;
                }
                Ok(response)
            }
            Err(error) => error.downcast_ref::<Fault>().map_or_else(
                || {
                    tracing::error!(event="fleet_command_failed",%error);
                    Response::error("fleet temporarily unavailable", 503)
                },
                |fault| routes::fault(fault).map_err(failure),
            ),
        }
    }

    pub(super) async fn reconcile_alarm(&self) {
        let outcome = self
            .reconcile_and_schedule(ReconcileMode::PendingOnly)
            .await;
        tracing::info!(event = "directory_kv_alarm_handled", ?outcome);
    }

    async fn reconcile_and_schedule(&self, mode: ReconcileMode) -> Option<ReconcileOutcome> {
        let _writer = self.publishing.lock().await;
        let outcome = match self.reconcile_locked(mode).await {
            Ok(outcome) => outcome,
            Err(error) => {
                tracing::warn!(event = "directory_kv_reconcile_failed", ?mode, %error);
                return None;
            }
        };
        if let Err(error) = self.sync_kv_alarm(outcome).await {
            tracing::warn!(event = "directory_kv_alarm_update_failed", ?outcome, %error);
        }
        Some(outcome)
    }

    async fn reconcile_locked(&self, mode: ReconcileMode) -> Result<ReconcileOutcome> {
        let mut db = Database::open(&self.state)?;
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
            storage
                .delete_alarm()
                .await
                .map_err(|error| anyhow::anyhow!("{error}"))?;
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
    match route {
        Route::Maintenance(action) => maintenance::dispatch(service, env, action, ctx, bytes),
        Route::Vantage(action) => dispatch_vantage(service, env, action, bytes),
        Route::Admin(action) => dispatch_admin(service, env, action, ctx, bytes),
        Route::Internal(action) => dispatch_internal(service, env, action, bytes),
    }
}

fn dispatch_vantage(
    service: &mut Service,
    env: &Env,
    route: VantageRoute,
    bytes: &[u8],
) -> Result<Value> {
    match route {
        VantageRoute::Targets => Ok(json!(service.fleet_targets(
            &agents(env)?,
            &routes::parse(bytes)?,
            origins()?
        )?)),
        VantageRoute::Cycle => {
            service.record_cycle(&agents(env)?, &routes::parse(bytes)?)?;
            Ok(json!({}))
        }
        VantageRoute::Poll => {
            let origins = origins()?;
            Ok(json!(service.claim(
                &agents(env)?,
                &routes::parse(bytes)?,
                origins
            )?))
        }
        VantageRoute::Result => {
            service.complete_probe(&agents(env)?, &routes::parse(bytes)?)?;
            Ok(json!({}))
        }
    }
}

fn dispatch_admin(
    service: &mut Service,
    env: &Env,
    route: AdminRoute,
    ctx: &Context,
    bytes: &[u8],
) -> Result<Value> {
    routes::verify_admin_csrf(&ctx.session, &ctx.csrf)?;
    service.require_admin(&ctx.session)?;
    let head = Publication::load_or_import(&mut service.store)?;
    match route {
        AdminRoute::Prepare => Ok(json!(head.envelope)),
        AdminRoute::Manage => {
            service.manage(&ctx.session, &routes::parse(bytes)?, &signer(env)?)?;
            Ok(json!({}))
        }
        AdminRoute::Stage => {
            let spec: Spec = routes::parse(bytes)?;
            anyhow::ensure!(
                !agents(env)?.iter().any(|agent| agent.key == spec.node_key),
                Fault::Conflict {
                    reason: "vantage_key_bound".into()
                }
            );
            let node = service.stage(&ctx.session, spec)?;
            Ok(json!(service.node_details(node)?))
        }
        AdminRoute::Promote => {
            #[derive(Deserialize)]
            struct Promotion {
                command: crate::fleet::Promote,
                observed_digest: String,
            }
            let Promotion {
                command,
                observed_digest,
            } = routes::parse(bytes)?;
            service.promote(
                &ctx.session,
                &command,
                &agents(env)?,
                &observed_digest,
                &signer(env)?,
            )?;
            Ok(json!({}))
        }
    }
}

fn dispatch_internal(
    service: &mut Service,
    env: &Env,
    route: InternalRoute,
    bytes: &[u8],
) -> Result<Value> {
    let mut head = Publication::load_or_import(&mut service.store)?;
    match route {
        InternalRoute::Tick => tick(service, env, &mut head),
        InternalRoute::Mirrors => {
            let check: MirrorCheck = routes::parse(bytes)?;
            if publication::digest(&head.envelope)? == check.digest {
                save(&mut service.store, MIRRORS, &check)?;
            }
            Ok(json!({}))
        }
    }
}

fn tick(service: &mut Service, env: &Env, head: &mut Publication) -> Result<Value> {
    let now = service.environment.now();
    let registry = agents(env);
    match &registry {
        Ok(agents) => {
            let missing = vantage::silent(&service.store, agents, now)?;
            if !missing.is_empty() {
                tracing::warn!(event="vantage_silent", vantages=?missing);
            }
        }
        Err(error) => tracing::error!(event="vantage_configuration_invalid", %error),
    }
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
            if let Ok(agents) = registry {
                service.evaluate_health(&agents, &publisher, head)?;
            }
            if head.renew(&publisher, now)? {
                head.save(&mut service.store)?;
            }
        }
        Err(error) => tracing::error!(event="directory_renewal_failed", %error),
    }
    Ok(json!(head.envelope))
}
