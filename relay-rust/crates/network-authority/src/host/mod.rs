#![allow(
    clippy::future_not_send,
    reason = "Workers run JS futures on one isolate thread"
)]
mod device_login;
mod edge;
mod mail;
mod routes;
mod storage;
use crate::{Authority, Environment, Fault, Signing};
use futures_util::StreamExt;
use storage::Database;
use worker::{DurableObject, Env, Method, Request, Response, Result, State, durable_object};

const MAX_REQUEST_BYTES: usize = windshare_relay_access::usage::MAX_REPORT_BYTES;
fn failure(error: impl std::fmt::Display) -> worker::Error {
    worker::Error::RustError(error.to_string())
}
struct Clock;
impl Environment for Clock {
    fn now(&self) -> u64 {
        worker::Date::now().as_millis() / 1000
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
}
impl DurableObject for NetworkAuthority {
    fn new(state: State, env: Env) -> Self {
        Self { state, env }
    }
    async fn fetch(&self, mut request: Request) -> Result<Response> {
        let mut bytes = Vec::new();
        if request.method() != Method::Get
            && let Ok(mut stream) = request.stream()
        {
            while let Some(chunk) = stream.next().await {
                let chunk = chunk?;
                if bytes.len() + chunk.len() > MAX_REQUEST_BYTES {
                    return Response::error("request too large", 413);
                }
                bytes.extend_from_slice(&chunk);
            }
        }
        if request.path() == "/v1/accounts/email" {
            return self.email(&request, &bytes).await;
        }
        let database = Database::open(&self.state).map_err(failure)?;
        let env = self.env.clone();
        let context = routes::Context::new(&request, &env)?;
        let result = storage::transaction(&self.state, move || {
            let signing = Signing::load(
                &env.var("AUTHORITY_ROOT")?.to_string(),
                &env.var("AUTHORITY_CERTIFICATE")?.to_string(),
                &env.secret("AUTHORITY_SIGNING_SEED")?.to_string(),
            )
            .map_err(|error| anyhow::anyhow!("{error}"))?;
            let mut authority = Authority::new(database, Clock, signing)?;
            match routes::dispatch(&mut authority, context, &bytes) {
                Ok(response) => Ok(response),
                Err(error) => {
                    if let Some(fault) = error.downcast_ref::<Fault>() {
                        return routes::fault(fault);
                    }
                    Err(error)
                }
            }
        });
        match result {
            Ok(response) => Ok(response),
            Err(error) => {
                worker::console_error!("authority_request_failed: {}", error);
                Response::error("authority temporarily unavailable", 503)
            }
        }
    }
}
