//! One management transaction owns enrollment, installation receipts and revocation.
mod database;
mod device_login;
#[cfg(test)]
#[path = "device_login_tests.rs"]
mod device_login_tests;
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
    node_key: ed25519_dalek::SigningKey,
    account_service: Option<crate::node::usage::Authority>,
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
        authority: Option<crate::node::usage::Authority>,
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
        access.restore_devices(database::keys(&db)?);
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
            node_key: identity.key(),
            account_service: authority,
            bootstrap: Digest::of(token.as_bytes()),
        }))))
    }
    pub(crate) fn origin(&self) -> &str {
        &self.0.origin
    }

    fn dispatch(
        &self,
        method: &Method,
        path_and_query: &str,
        body: &Value,
        session: &str,
        now: u64,
    ) -> Result<Reply> {
        let (path, query) = path_and_query
            .split_once('?')
            .unwrap_or((path_and_query, ""));
        let mut db = self
            .0
            .db
            .lock()
            .map_err(|_| anyhow!("console store poisoned"))?;
        let mut tx = database::Management::begin(&mut db)?;
        database::cleanup(&tx.db, now)?;
        let result = match (method, path) {
            (&Method::POST, "/api/initialize") => self.initialize(&tx.db, body, now),
            (&Method::POST, "/api/login") => Self::login(&tx.db, body, now),
            (&Method::POST, "/api/enrollments/account") => self.approve_account(&tx.db, body, now),
            (&Method::POST, path) if claimant(path) => self.device(&mut tx, path, body, now),
            _ if !sessions::authenticated(&tx.db, session, now)? => {
                Err(Rejected(StatusCode::UNAUTHORIZED, "login_required", 0).into())
            }
            (&Method::POST, "/api/logout") => Self::logout(&tx.db, session),
            (&Method::GET, "/api/session") => Ok(Reply::ok(
                json!({"authenticated":true,"capabilities":{"revocation_stops_shares":true}}),
            )),
            _ => self.manage(&mut tx, method, path, query, body, now),
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
        // Hold the database guard through publication so runtime order matches durable order.
        if let Some(credential) = tx.commit()? {
            let id = credential.key.id;
            self.0.access.apply_device(credential);
            tracing::debug!(event = "device_credential_published", credential_id = ?id);
        }
        drop(db);
        tracing::debug!(event="node_console_request",path,status=reply.status.as_u16(),credential_id=?body.get("credential_id"),enrollment_id=?body.get("enrollment_id"));
        Ok(reply)
    }
}
