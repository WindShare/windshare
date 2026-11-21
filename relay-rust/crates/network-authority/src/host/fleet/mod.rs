mod edge;
mod maintenance;
mod public_directory;
mod public_dns;
mod reconcile;
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
use reconcile::{DirectoryMirror, ReconcileMode};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use worker::{Env, Method, Request, Response, ScheduleContext, ScheduledEvent, event};
const DIRECTORY_PATH: &str = "/.well-known/windshare-network";
const MIRRORS: &str = "directory/mirrors";
const KV_PUBLISHED_HEADER: &str = "X-Directory-Published";
// Cloudflare Workers KV enforces a 60 s minimum cache_ttl on reads.
const KV_AUDIT_CACHE_SECONDS: u64 = 60;
const EXPIRY_ALERT_SECONDS: u64 = 14 * 86400;

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
    command: &str,
    auth: (Option<&str>, Option<&str>),
    body: &impl Serialize,
) -> worker::Result<Response> {
    call_do_json_with_headers(
        env,
        &format!("/internal/fleet/{command}"),
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
    let mut response = internal(env, "tick", (None, None), &json!({})).await?;
    if response.status_code() != 200 {
        return Err(failure("directory maintenance unavailable"));
    }
    if response.headers().get(KV_PUBLISHED_HEADER)?.as_deref() == Some("1") {
        // Frontend mirrors cache KV reads for 60 s, so probing milliseconds
        // after a fresh KV write always sees the previous revision.
        tracing::info!(
            event = "directory_mirror_check_deferred",
            reason = "kv_just_published"
        );
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
    let response = internal(env, "mirrors", (None, None), &check).await?;
    if response.status_code() != 200 {
        return Err(failure("mirror report unavailable"));
    }
    Ok(())
}
impl NetworkAuthority {
    pub(super) async fn fleet(&self, req: &Request, bytes: &[u8]) -> worker::Result<Response> {
        if req.method() != Method::Post {
            return Response::error("not found", 404);
        }
        let ctx = Context::new(req, &self.env)?;
        let bytes = bytes.to_vec();
        let is_tick = ctx.path == "/internal/fleet/tick";
        let publish = is_tick
            || matches!(
                ctx.path.as_str(),
                "/internal/fleet/promote"
                    | "/v1/admin/nodes/manage"
                    | "/v1/maintenance/drain"
                    | "/v1/maintenance/release"
            );
        // A rejected fleet command rolls back the whole transaction, including
        // bootstrap and lease writes. Network reads and KV writes run outside it.
        let result = self.transact(
            WorkerEnvironment {
                registration_email_configured: false,
            },
            move |service, env| dispatch(service, env, &ctx, &bytes),
        );
        // A failed health or renewal transaction cannot strand the last committed head in SQLite.
        let mode = if is_tick {
            ReconcileMode::AuditKv
        } else {
            ReconcileMode::PendingOnly
        };
        let published = if publish && (result.is_ok() || is_tick) {
            match self.reconcile(mode).await {
                Ok(written) => written,
                Err(error) => {
                    tracing::warn!(event="directory_kv_retry_pending", ?mode, %error);
                    false
                }
            }
        } else {
            false
        };
        match result {
            Ok(value) => {
                let mut response = Response::from_json(&value)?;
                if published && is_tick {
                    response.headers_mut().set(KV_PUBLISHED_HEADER, "1")?;
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
    async fn reconcile(&self, mode: ReconcileMode) -> worker::Result<bool> {
        let _writer = self.publishing.lock().await;
        let mut db = Database::open(&self.state).map_err(failure)?;
        let mirror = KvMirror(self.env.kv("DIRECTORY")?);
        reconcile::reconcile(&mut db, &mirror, mode)
            .await
            .map_err(failure)
    }
}

fn dispatch(service: &mut Service, env: &Env, ctx: &Context, bytes: &[u8]) -> Result<Value> {
    let admin = matches!(
        ctx.path.as_str(),
        "/internal/fleet/prepare"
            | "/internal/fleet/stage"
            | "/internal/fleet/promote"
            | "/v1/admin/nodes/manage"
    );
    if admin {
        routes::verify_admin_csrf(&ctx.session, &ctx.csrf)?;
        service.require_admin(&ctx.session)?;
    }
    if ctx.path.starts_with("/v1/maintenance/") {
        return maintenance::dispatch(service, env, ctx, bytes);
    }
    match ctx.path.as_str() {
        "/v1/vantage/targets" => {
            return Ok(json!(service.fleet_targets(
                &agents(env)?,
                &routes::parse(bytes)?,
                origins()?
            )?));
        }
        "/v1/vantage/cycle" => {
            service.record_cycle(&agents(env)?, &routes::parse(bytes)?)?;
            return Ok(json!({}));
        }
        "/v1/vantage/poll" => {
            let origins = origins()?;
            return Ok(json!(service.claim(
                &agents(env)?,
                &routes::parse(bytes)?,
                origins
            )?));
        }
        "/v1/vantage/result" => {
            service.complete_probe(&agents(env)?, &routes::parse(bytes)?)?;
            return Ok(json!({}));
        }
        _ => {}
    }
    let mut head = Publication::load_or_import(&mut service.store)?;
    match ctx.path.as_str() {
        "/internal/fleet/prepare" => Ok(json!(head.envelope)),
        "/v1/admin/nodes/manage" => {
            service.manage(&ctx.session, &routes::parse(bytes)?, &signer(env)?)?;
            Ok(json!({}))
        }
        "/internal/fleet/stage" => {
            let spec: Spec = routes::parse(bytes)?;
            anyhow::ensure!(
                !agents(env)?.iter().any(|agent| agent.key == spec.node_key),
                Fault::Invalid
            );
            let node = service.stage(&ctx.session, spec)?;
            Ok(json!(service.node_details(node)?))
        }
        "/internal/fleet/promote" => {
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
        "/internal/fleet/tick" => tick(service, env, &mut head),
        "/internal/fleet/mirrors" => {
            let check: MirrorCheck = routes::parse(bytes)?;
            if publication::digest(&head.envelope)? == check.digest {
                save(&mut service.store, MIRRORS, &check)?;
            }
            Ok(json!({}))
        }
        _ => Err(Fault::Invalid.into()),
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
