#![allow(
    clippy::future_not_send,
    reason = "Workers run JS futures on one isolate thread"
)]
mod device_login;
mod edge;
mod fleet;
mod mail;
mod routes;
mod storage;
mod telemetry;
use crate::{
    Authority, Environment, Fault, NodeLoadReport, PepperRing, Signing,
    public_blocks::PublicBlockFeed,
};
use futures_util::StreamExt;
use std::{
    cell::{Cell, OnceCell, RefCell},
    collections::BTreeMap,
    rc::Rc,
};
use storage::Database;
use worker::{DurableObject, Env, Method, Request, Response, Result, State, durable_object};

const MAX_REQUEST_BYTES: usize = windshare_relay_access::usage::MAX_REPORT_BYTES;
fn failure(error: impl std::fmt::Display) -> worker::Error {
    worker::Error::RustError(error.to_string())
}

async fn read_body(request: &mut Request) -> Result<Option<Vec<u8>>> {
    let mut bytes = Vec::new();
    if let Ok(mut stream) = request.stream() {
        while let Some(chunk) = stream.next().await {
            let chunk = chunk?;
            if bytes.len() + chunk.len() > MAX_REQUEST_BYTES {
                return Ok(None);
            }
            bytes.extend_from_slice(&chunk);
        }
    }
    Ok(Some(bytes))
}
#[derive(Clone)]
struct WorkerEnvironment {
    registration_email_configured: bool,
    peppers: PepperRing,
}

impl Environment for WorkerEnvironment {
    fn now(&self) -> u64 {
        worker::Date::now().as_millis() / 1000
    }
    fn registration_email_configured(&self) -> bool {
        self.registration_email_configured
    }
    fn password_peppers(&self) -> &PepperRing {
        &self.peppers
    }

    fn random<const N: usize>(&mut self) -> anyhow::Result<[u8; N]> {
        let mut bytes = [0; N];
        getrandom::fill(&mut bytes).map_err(|error| anyhow::anyhow!("{error}"))?;
        Ok(bytes)
    }
}
#[durable_object]
pub struct NetworkAuthority {
    state: State,
    env: Env,
    signing: OnceCell<Signing>,
    peppers: OnceCell<PepperRing>,
    schema_ready: Cell<bool>,
    public_blocks: Rc<RefCell<PublicBlockFeed>>,
    publishing: futures_util::lock::Mutex<()>,
    kv_alarm_deadline: Cell<Option<u64>>,
    node_loads: Rc<RefCell<BTreeMap<[u8; 32], NodeLoadReport>>>,
}
impl NetworkAuthority {
    pub(super) fn database(&self) -> anyhow::Result<Database> {
        let database = Database::new(&self.state);
        if !self.schema_ready.get() {
            // Run table and index DDL once per DO instance outside rollbackable
            // transactions so online requests never run DDL or lose schema setup on abort.
            database.ensure_schema()?;
            self.schema_ready.set(true);
            tracing::debug!(event = "authority_schema_ready");
        }
        Ok(database)
    }

    pub(super) fn signing(&self) -> anyhow::Result<Signing> {
        if let Some(signing) = self.signing.get() {
            return Ok(signing.clone());
        }
        // Worker vars and secrets are immutable for the lifetime of a Durable Object
        // instance; verify the root-signed online certificate once and reuse it while
        // Authority::new still enforces time-based expiry and rollback barriers.
        let signing = Signing::load(
            &self.env.var("AUTHORITY_ROOT")?.to_string(),
            &self.env.var("AUTHORITY_CERTIFICATE")?.to_string(),
            &self.env.secret("AUTHORITY_SIGNING_SEED")?.to_string(),
        )?;
        let generation = signing.generation.number;
        tracing::debug!(event = "authority_signing_loaded", generation);
        Ok(self.signing.get_or_init(|| signing).clone())
    }

    pub(super) fn peppers(&self) -> anyhow::Result<PepperRing> {
        if let Some(peppers) = self.peppers.get() {
            return Ok(peppers.clone());
        }
        let peppers = PepperRing::parse(&self.env.secret("PASSWORD_PEPPER")?.to_string())?;
        tracing::debug!(
            event = "authority_password_pepper_loaded",
            active = peppers.active_id(),
            count = peppers.len()
        );
        Ok(self.peppers.get_or_init(|| peppers).clone())
    }

    // Keep live isolate load state synchronized across every DO transaction so
    // uncheckpointed node heartbeats are never lost between HTTP entry points.
    fn transact<T: 'static>(
        &self,
        registration_email_configured: bool,
        action: impl FnOnce(&mut Authority<Database, WorkerEnvironment>, &Env) -> anyhow::Result<T>
        + 'static,
    ) -> anyhow::Result<T> {
        let signing = self.signing()?;
        let peppers = self.peppers()?;
        let database = self.database()?;
        let env = self.env.clone();
        let loads = self.node_loads.borrow().clone();
        let environment = WorkerEnvironment {
            registration_email_configured,
            peppers,
        };
        let (value, updated_loads) = storage::transaction(&self.state, move || {
            let mut authority = Authority::new(database, environment, signing)?;
            authority.set_live_loads(loads);
            let value = action(&mut authority, &env)?;
            Ok((value, authority.into_live_loads()))
        })?;
        *self.node_loads.borrow_mut() = updated_loads;
        Ok(value)
    }
}
impl DurableObject for NetworkAuthority {
    fn new(state: State, env: Env) -> Self {
        telemetry::install(&env);
        Self {
            state,
            env,
            signing: OnceCell::new(),
            peppers: OnceCell::new(),
            schema_ready: Cell::new(false),
            public_blocks: Rc::default(),
            node_loads: Rc::default(),
            publishing: futures_util::lock::Mutex::new(()),
            kv_alarm_deadline: Cell::new(None),
        }
    }
    async fn fetch(&self, mut request: Request) -> Result<Response> {
        let bytes = if request.method() == Method::Get {
            Vec::new()
        } else if let Some(bytes) = read_body(&mut request).await? {
            bytes
        } else {
            return Response::error("request too large", 413);
        };
        if let Some(route) = fleet::Route::parse(&request.path()) {
            return self.fleet(route, &request, &bytes).await;
        }
        match request.path().as_str() {
            "/v1/accounts/email" => return self.email(&request, &bytes, None).await,
            "/v1/accounts/profile/email" => {
                let bearer = routes::bearer(&request)?;
                return self.email(&request, &bytes, Some(bearer)).await;
            }
            _ => {}
        }
        let context = routes::Context::new(&request, &self.env)?;
        let registration_email_configured = mail::Webhook::configured(&self.env).is_some();
        let public_blocks = self.public_blocks.clone();
        match self.transact(registration_email_configured, move |authority, _| {
            match routes::dispatch(authority, context, &bytes, &mut public_blocks.borrow_mut()) {
                Ok(response) => Ok(response),
                Err(error) => {
                    if let Some(fault) = error.downcast_ref::<Fault>() {
                        return routes::fault(fault);
                    }
                    Err(error)
                }
            }
        }) {
            Ok(response) => Ok(response),
            Err(error) => {
                tracing::error!(event = "authority_request_failed", %error);
                Response::error("authority temporarily unavailable", 503)
            }
        }
    }

    async fn alarm(&self) -> Result<Response> {
        // Clear the cached deadline before awaiting the publishing lock so any
        // concurrent request re-arms from authoritative alarm storage.
        self.kv_alarm_deadline.set(None);
        self.reconcile_alarm().await;
        // Return Ok after handling and re-arming so the Workers runtime does not
        // layer its own unbounded alarm retries on top of RETRY_BACKOFF_MS.
        Response::empty()
    }
}
