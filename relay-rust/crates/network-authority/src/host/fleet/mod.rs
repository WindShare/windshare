mod edge;
mod public_directory;
mod public_dns;
use super::{
    NetworkAuthority, WorkerEnvironment, failure,
    passwords::{ForwardHeaders, call_do_json_with_headers},
    routes::{self, Context},
    storage::{self, Database},
};
use crate::{
    Authority, Environment, Fault, Signing, Store,
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
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use worker::{Env, Method, Request, Response, ScheduleContext, ScheduledEvent, event};
const DIRECTORY_PATH: &str = "/.well-known/windshare-network";
const MIRRORS: &str = "directory/mirrors";
const DELIVERED: &str = "directory/kv-digest";
const EXPIRY_ALERT_SECONDS: u64 = 14 * 86400;
fn release() -> Result<Release> {
    Release::bundled()
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
    let expected: Envelope = response.json().await?;
    let matches = edge::mirrors_match(&expected).await;
    let check = MirrorCheck {
        digest: publication::digest(&expected).map_err(failure)?,
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
        let database = Database::open(&self.state).map_err(failure)?;
        let env = self.env.clone();
        let bytes = bytes.to_vec();
        let publish = matches!(
            ctx.path.as_str(),
            "/internal/fleet/promote" | "/internal/fleet/tick"
        );
        // A rejected fleet command rolls back the whole transaction, including
        // bootstrap and lease writes. Network reads and KV writes run outside it.
        let result = storage::transaction(&self.state, move || {
            let signing = Signing::load(
                &env.var("AUTHORITY_ROOT")?.to_string(),
                &env.var("AUTHORITY_CERTIFICATE")?.to_string(),
                &env.secret("AUTHORITY_SIGNING_SEED")?.to_string(),
            )?;
            let mut service = Authority::new(
                database,
                WorkerEnvironment {
                    registration_email_configured: false,
                },
                signing,
            )?;
            dispatch(&mut service, &env, &ctx, &bytes)
        });
        match result {
            Ok(value) => {
                if publish && let Err(error) = self.reconcile().await {
                    tracing::warn!(event="directory_kv_retry_pending",%error);
                }
                Response::from_json(&value)
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
    async fn reconcile(&self) -> worker::Result<()> {
        let _writer = self.publishing.lock().await;
        let mut db = Database::open(&self.state).map_err(failure)?;
        let head = publication::current(&db).map_err(failure)?;
        let digest = publication::digest(&head.envelope).map_err(failure)?;
        let kv = self.env.kv("DIRECTORY")?;
        let body = serde_json::to_string(&head.envelope)?;
        // A namespace can be emptied or replaced independently of SQLite. Audit the
        // delivery on the scheduled path so public reads never need the DO.
        if db.get(DELIVERED).map_err(failure)?.as_deref() == Some(&digest)
            && kv.get(KV_KEY).cache_ttl(60).text().await?.as_deref() == Some(body.as_str())
        {
            return Ok(());
        }
        kv.put(KV_KEY, body)?.execute().await?;
        db.put(DELIVERED, &digest).map_err(failure)?;
        tracing::info!(event="directory_kv_published",operation_id=%digest);
        Ok(())
    }
}

fn dispatch(service: &mut Service, env: &Env, ctx: &Context, bytes: &[u8]) -> Result<Value> {
    let admin = matches!(
        ctx.path.as_str(),
        "/internal/fleet/prepare" | "/internal/fleet/stage" | "/internal/fleet/promote"
    );
    if admin {
        routes::verify_admin_csrf(&ctx.session, &ctx.csrf)?;
        service.require_admin(&ctx.session)?;
    }
    match ctx.path.as_str() {
        "/v1/vantage/poll" => {
            let release = release()?;
            let origins = [
                release.link_host,
                release.backup_entry,
                "https://self-hosted.example".into(),
            ];
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
        "/internal/fleet/tick" => {
            let now = service.environment.now();
            match signer(env).and_then(|publisher| head.renew(&publisher, now)) {
                Ok(true) => head.save(&mut service.store)?,
                Ok(false) => {}
                Err(error) => tracing::error!(event="directory_renewal_failed", %error),
            }
            match agents(env) {
                Ok(registry) => {
                    let missing = vantage::silent(&service.store, &registry, now)?;
                    if !missing.is_empty() {
                        tracing::warn!(event="vantage_silent",vantages=?missing);
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
            Ok(json!(head.envelope))
        }
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
