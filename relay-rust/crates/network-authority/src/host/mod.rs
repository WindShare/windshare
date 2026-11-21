#![allow(
    clippy::future_not_send,
    reason = "Workers run JS futures on one isolate thread"
)]
mod device_login;
mod edge;
mod internal_routes;
mod mail;
mod passwords;
mod routes;
mod storage;
mod telemetry;
use crate::{
    Authority, Environment, Fault, NodeLoadReport, Signing, public_blocks::PublicBlockFeed,
};
use futures_util::StreamExt;
use std::{cell::RefCell, collections::BTreeMap, rc::Rc};
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
#[derive(Clone, Copy)]
struct WorkerEnvironment {
    registration_email_configured: bool,
}

impl Environment for WorkerEnvironment {
    fn now(&self) -> u64 {
        worker::Date::now().as_millis() / 1000
    }
    fn registration_email_configured(&self) -> bool {
        self.registration_email_configured
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
    public_blocks: Rc<RefCell<PublicBlockFeed>>,
    node_loads: Rc<RefCell<BTreeMap<[u8; 32], NodeLoadReport>>>,
}
impl DurableObject for NetworkAuthority {
    fn new(state: State, env: Env) -> Self {
        telemetry::install(&env);
        Self {
            state,
            env,
            public_blocks: Rc::default(),
            node_loads: Rc::default(),
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
        match request.path().as_str() {
            "/v1/accounts/email" => return self.email(&request, &bytes, None).await,
            "/v1/accounts/profile/email" => {
                let bearer = routes::bearer(&request)?;
                return self.email(&request, &bytes, Some(bearer)).await;
            }
            _ => {}
        }
        let database = Database::open(&self.state).map_err(failure)?;
        let env = self.env.clone();
        let context = routes::Context::new(&request, &env)?;
        let environment = WorkerEnvironment {
            registration_email_configured: mail::Webhook::configured(&env).is_some(),
        };
        let public_blocks = self.public_blocks.clone();
        let initial_loads = self.node_loads.borrow().clone();
        let result = storage::transaction(&self.state, move || {
            let signing = Signing::load(
                &env.var("AUTHORITY_ROOT")?.to_string(),
                &env.var("AUTHORITY_CERTIFICATE")?.to_string(),
                &env.secret("AUTHORITY_SIGNING_SEED")?.to_string(),
            )
            .map_err(|error| anyhow::anyhow!("{error}"))?;
            let mut authority = Authority::new(database, environment, signing)?;
            authority.set_live_loads(initial_loads);
            match routes::dispatch(
                &mut authority,
                context,
                &bytes,
                &mut public_blocks.borrow_mut(),
            ) {
                Ok(response) => Ok((response, authority.into_live_loads())),
                Err(error) => {
                    if let Some(fault) = error.downcast_ref::<Fault>() {
                        return routes::fault(fault)
                            .map(|response| (response, authority.into_live_loads()));
                    }
                    Err(error)
                }
            }
        });
        match result {
            Ok((response, updated_loads)) => {
                *self.node_loads.borrow_mut() = updated_loads;
                Ok(response)
            }
            Err(error) => {
                tracing::error!(event = "authority_request_failed", %error);
                Response::error("authority temporarily unavailable", 503)
            }
        }
    }
}
