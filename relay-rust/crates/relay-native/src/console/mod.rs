//! One management transaction owns enrollment, installation receipts and revocation.
mod database;
mod devices;
mod http;
mod sessions;
#[cfg(test)]
#[path = "console_tests.rs"]
mod tests;

use crate::{
    Database, Endpoint,
    node::{Identity, access::Service},
};
use anyhow::{Result, anyhow};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use hyper::{Method, StatusCode};
use rusqlite::Connection;
use serde_json::{Value, json};
use std::sync::{Arc, Mutex};
use windshare_relay_access::{Digest, POLL_INTERVAL_MS, Refusal};

#[derive(Clone)]
pub struct Console(Arc<Inner>);
struct Inner {
    db: Mutex<Connection>,
    access: Service,
    origin: String,
    relay: String,
    owner: [u8; 16],
    bootstrap: Digest,
}

struct Reply {
    status: StatusCode,
    body: Value,
    cookie: Option<String>,
}

impl Reply {
    const fn ok(body: Value) -> Self {
        Self {
            status: StatusCode::OK,
            body,
            cookie: None,
        }
    }
    fn error(status: StatusCode, code: &str) -> Self {
        Self {
            status,
            body: json!({"error":code}),
            cookie: None,
        }
    }
}

#[derive(Debug)]
struct Rejected(StatusCode, &'static str, u64);
impl std::fmt::Display for Rejected {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(self.1)
    }
}
impl std::error::Error for Rejected {}

const fn refusal(err: Refusal) -> Rejected {
    match err {
        Refusal::AccessDenied | Refusal::Cancelled => {
            Rejected(StatusCode::FORBIDDEN, "access_denied", 0)
        }
        Refusal::Expired => Rejected(StatusCode::GONE, "expired_token", 0),
        Refusal::Pending => Rejected(
            StatusCode::ACCEPTED,
            "authorization_pending",
            POLL_INTERVAL_MS,
        ),
        Refusal::SlowDown { retry_after_ms } => {
            Rejected(StatusCode::TOO_MANY_REQUESTS, "slow_down", retry_after_ms)
        }
        Refusal::AlreadyCompleted => Rejected(StatusCode::CONFLICT, "already_completed", 0),
        Refusal::InvalidName => invalid(),
    }
}
const fn invalid() -> Rejected {
    Rejected(StatusCode::BAD_REQUEST, "invalid_request", 0)
}
fn field<'a>(value: &'a Value, name: &str) -> Result<&'a str> {
    value
        .get(name)
        .and_then(Value::as_str)
        .ok_or_else(|| invalid().into())
}
fn id<const N: usize>(value: &Value, name: &str) -> Result<[u8; N]> {
    URL_SAFE_NO_PAD
        .decode(field(value, name)?)
        .ok()
        .and_then(|v| v.try_into().ok())
        .ok_or_else(|| invalid().into())
}
fn random<const N: usize>() -> Result<[u8; N]> {
    let mut bytes = [0; N];
    getrandom::fill(&mut bytes).map_err(|e| anyhow!("console entropy: {e}"))?;
    Ok(bytes)
}
fn claimant(path: &str) -> bool {
    matches!(
        path,
        "/api/enrollments"
            | "/api/enrollments/claim"
            | "/api/enrollments/installed"
            | "/api/enrollments/cancel"
    )
}

impl Console {
    pub(crate) fn open(
        endpoint: &Endpoint,
        store: &Database,
        access: Service,
        identity: &Identity,
    ) -> Result<Option<Self>> {
        let url = url::Url::parse(endpoint.relay_url())?;
        if url.scheme() != "https"
            && !matches!(url.host(), Some(url::Host::Domain("localhost")))
            && !url.host_str().is_some_and(|s| {
                s.trim_matches(['[', ']'])
                    .parse::<std::net::IpAddr>()
                    .is_ok_and(|a| a.is_loopback())
            })
        {
            return Ok(None);
        }
        let origin = url.origin().ascii_serialization();
        let db = store.console_connection()?;
        database::initialize(&db)?;
        access.apply_devices(database::keys(&db)?);
        let token = identity.initialization_token();
        if !sessions::initialized(&db)? {
            println!(
                "{}",
                json!({"event":"node_console_setup","node_id":identity.id(),"url":format!("{origin}/#initialize={token}")})
            );
        }
        Ok(Some(Self(Arc::new(Inner {
            db: Mutex::new(db),
            access,
            origin,
            relay: endpoint.relay_url().to_owned(),
            owner: identity.owner().principal,
            bootstrap: Digest::of(token.as_bytes()),
        }))))
    }
    pub(crate) fn origin(&self) -> &str {
        &self.0.origin
    }

    fn dispatch(
        &self,
        method: &Method,
        path: &str,
        body: &Value,
        session: &str,
        now: u64,
    ) -> Result<Reply> {
        let mut db = self
            .0
            .db
            .lock()
            .map_err(|_| anyhow!("console store poisoned"))?;
        let tx = db.transaction()?;
        database::cleanup(&tx, now)?;
        let result = match (method, path) {
            (&Method::POST, "/api/initialize") => self.initialize(&tx, body, now),
            (&Method::POST, "/api/login") => Self::login(&tx, body, now),
            (&Method::POST, path) if claimant(path) => self.device(&tx, path, body, now),
            _ if !sessions::authenticated(&tx, session, now)? => {
                Err(Rejected(StatusCode::UNAUTHORIZED, "login_required", 0).into())
            }
            (&Method::POST, "/api/logout") => Self::logout(&tx, session),
            (&Method::GET, "/api/session") => Ok(Reply::ok(
                json!({"authenticated":true,"capabilities":{"revocation_stops_shares":true}}),
            )),
            _ => self.manage(&tx, method, path, body, now),
        };
        let reply = match result {
            Ok(reply) => reply,
            Err(e) => {
                let Some(Rejected(status, reason, retry)) = e.downcast_ref::<Rejected>() else {
                    return Err(e);
                };
                Reply {
                    status: *status,
                    body: json!({"error":reason,"retry_after_ms":retry}),
                    cookie: None,
                }
            }
        };
        let changed = reply.status.is_success()
            && *method == Method::POST
            && matches!(
                path,
                "/api/enrollments/installed"
                    | "/api/enrollments/cancel"
                    | "/api/credentials"
                    | "/api/credentials/revoke"
            );
        let keys = changed.then(|| database::keys(&tx)).transpose()?;
        tx.commit()?;
        // Serialize publication with the durable write so concurrent management
        // requests cannot restore an older access snapshot after revocation.
        if let Some(keys) = keys {
            self.0.access.apply_devices(keys);
        }
        drop(db);
        tracing::debug!(event="node_console_request",path,status=reply.status.as_u16(),credential_id=?body.get("credential_id"),enrollment_id=?body.get("enrollment_id"));
        Ok(reply)
    }
}
