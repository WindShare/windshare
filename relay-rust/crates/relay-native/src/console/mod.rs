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
use hyper::{Method, StatusCode};
use rusqlite::Connection;
use serde_json::{Value, json};
use std::sync::{Arc, Mutex};
use windshare_relay_access::Digest;
use windshare_relay_console::http::{ConsoleError, is_claimant_endpoint};

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

enum Outcome<'a> {
    Reply(Reply),
    ViewContribution(&'a crate::node::Runtime),
    SetContribution(&'a crate::node::Runtime, crate::node::contribution::Policy),
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
            body: json!({"error": code}),
            cookie: None,
        }
    }
    fn from_console_error(error: ConsoleError) -> Self {
        Self {
            status: StatusCode::from_u16(error.status).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR),
            body: error.body(),
            cookie: None,
        }
    }
}

fn random<const N: usize>() -> Result<[u8; N]> {
    let mut bytes = [0; N];
    getrandom::fill(&mut bytes).map_err(|e| anyhow!("console entropy: {e}"))?;
    Ok(bytes)
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
        let loopback = match url.host() {
            Some(url::Host::Domain("localhost")) => true,
            Some(url::Host::Ipv4(ip)) => ip.is_loopback(),
            Some(url::Host::Ipv6(ip)) => ip.is_loopback(),
            _ => false,
        };
        if url.scheme() != "https" && !loopback {
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
                json!({"event": "node_console_setup", "node_id": identity.id(), "url": format!("{origin}/#initialize={token}")})
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
        node: Option<&crate::node::Runtime>,
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
        let contribution = node.filter(|runtime| runtime.has_local_contribution());
        let result = match (method, path) {
            (&Method::POST, "/api/initialize") => {
                self.initialize(&tx.db, body, now).map(Outcome::Reply)
            }
            (&Method::GET, "/api/session") | (&Method::POST, "/api/login")
                if !sessions::initialized(&tx.db)? =>
            {
                Ok(Outcome::Reply(Reply::error(
                    StatusCode::UNAUTHORIZED,
                    "uninitialized",
                )))
            }
            (&Method::POST, "/api/login") => Self::login(&tx.db, body, now).map(Outcome::Reply),
            (&Method::POST, "/api/enrollments/account") => {
                self.approve_account(&tx.db, body, now).map(Outcome::Reply)
            }
            (&Method::POST, path) if is_claimant_endpoint(path) => {
                self.device(&mut tx, path, body, now).map(Outcome::Reply)
            }
            _ if !sessions::authenticated(&tx.db, session, now)? => {
                Err(ConsoleError::unauthorized("login_required").into())
            }
            (&Method::POST, "/api/logout") => Self::logout(&tx.db, session).map(Outcome::Reply),
            (&Method::POST, "/api/password") => {
                Self::change_password(&tx.db, body, now).map(Outcome::Reply)
            }
            (&Method::GET, "/api/session") => Ok(Outcome::Reply(Reply::ok(json!({
                "authenticated": true,
                "capabilities": {
                    "revocation_stops_shares": true,
                    "password_change": true,
                    "contribution": contribution.is_some(),
                }
            })))),
            (&Method::GET, "/api/contribution") if let Some(runtime) = contribution => {
                Ok(Outcome::ViewContribution(runtime))
            }
            (&Method::POST, "/api/contribution") if let Some(runtime) = contribution => {
                match serde_json::from_value::<crate::node::contribution::Policy>(body.clone()) {
                    Ok(policy) if policy.validate().is_ok() => {
                        Ok(Outcome::SetContribution(runtime, policy))
                    }
                    _ => Ok(Outcome::Reply(Reply::error(
                        StatusCode::BAD_REQUEST,
                        "invalid_request",
                    ))),
                }
            }
            _ => self
                .manage(&mut tx, method, path, query, body, now)
                .map(Outcome::Reply),
        };
        let outcome = match result {
            Ok(outcome) => outcome,
            Err(error) => {
                let Some(console_error) = error.downcast_ref::<ConsoleError>() else {
                    return Err(error);
                };
                Outcome::Reply(Reply::from_console_error(*console_error))
            }
        };
        // Hold the database guard through publication so runtime order matches durable order.
        if let Some(credential) = tx.commit()? {
            let id = credential.key.id;
            self.0.access.apply_device(credential);
            tracing::debug!(event = "device_credential_published", credential_id = ?id);
        }
        drop(db);
        // Apply contribution actions outside `.console.sqlite` so console auth never nests
        // with `Runtime::control_writer` or `node.sqlite`.
        let reply = match outcome {
            Outcome::Reply(reply) => reply,
            Outcome::ViewContribution(runtime) => Reply::ok(runtime.contribution_view()?),
            Outcome::SetContribution(runtime, policy) => {
                let disabled = runtime.set_contribution(policy, i64::try_from(now / 1_000)?)?;
                Reply::ok(json!({"disabled_notice": disabled}))
            }
        };
        tracing::debug!(
            event = "node_console_request",
            path,
            status = reply.status.as_u16(),
            credential_id = ?body.get("credential_id"),
            enrollment_id = ?body.get("enrollment_id")
        );
        Ok(reply)
    }
}
