mod analytics;
mod credentials;
mod provider;
mod provisioning;
mod revocation;

use super::{
    AccessObject, Database, Digest, failure, io, parse_id, reply, required, text_id, trace,
};
use crate::turn::{Lease, LeaseState, Provisioning};
use serde_json::{Value, json};
use windshare_relay_access::{Credential, CredentialState};
use worker::{Method, Request, Response, Result};

const PROVISION_KEY: &str = "turn:provision";
const ENABLED_KEY: &str = "turn:enabled";
const LEASE_PREFIX: &str = "turn_lease:";
const RETENTION_MS: u64 = 60 * 60 * 1000;
const MAX_BODY_BYTES: usize = 4096;

fn lease_key(id: &str) -> String {
    format!("{LEASE_PREFIX}{id}")
}
fn save(db: &Database, lease: &Lease) -> Result<()> {
    db.put(
        &lease_key(&lease.id),
        lease,
        Some(lease.expires_at.saturating_add(RETENTION_MS)),
    )
}
fn enabled(db: &Database) -> Result<bool> {
    Ok(db.get::<bool>(ENABLED_KEY)?.unwrap_or(true))
}

impl AccessObject {
    fn turn_deployment_id(&self) -> Result<String> {
        let deployment: Value =
            serde_json::from_str(&self.env.var("WINDSHARE_DEPLOYMENT")?.to_string())?;
        Ok(required(&deployment, "deploymentId")?.to_string())
    }
    fn turn_service_id(&self) -> Result<String> {
        let id = self.turn_deployment_id()?;
        Ok(format!(
            "cf-{}",
            text_id(&Digest::of(id.as_bytes()).0[..16])
        ))
    }
    fn turn_ready(&self, db: &Database) -> Result<bool> {
        let Some(state) = db.get::<Provisioning>(PROVISION_KEY)? else {
            return Ok(false);
        };
        Ok(state.published()
            && provider::secret(&self.env)
                .is_ok_and(|secret| Some(secret.key_id.as_str()) == state.key_id()))
    }
    pub(super) fn turn_reference(&self, db: &Database) -> Result<Value> {
        if !self.turn_ready(db)? {
            return Ok(Value::Null);
        }
        Ok(
            json!({"service_id":self.turn_service_id()?,"relay_url":self.env.var("RELAY_URL")?.to_string(),
            "enabled":enabled(db)?,"local_budget_bytes":0}),
        )
    }
    pub(super) async fn turn_device(&self, mut req: Request) -> Result<Response> {
        let db = Database::open(&self.state)?;
        let now = io::now().0;
        // Authenticate the current request, never a device ID in its body or a
        // previous WebSocket admission. Revoked owners may still request cleanup.
        let digest = req.headers().get("Authorization")?.and_then(|header| {
            header
                .strip_prefix("Bearer ")
                .map(|key| Digest::of(key.as_bytes()))
        });
        let Some(mut credential) = digest
            .map(|value| db.credential(value))
            .transpose()?
            .flatten()
        else {
            return reply(&json!({"error":"access_denied"}), 403);
        };
        let path = req.path();
        let revoke = path == "/api/turn/revoke";
        if !revoke && credential.state != CredentialState::Active {
            return reply(&json!({"error":"access_denied"}), 403);
        }
        if req.method() == Method::Get && path == "/api/turn/capability" {
            if !self.turn_ready(&db)? {
                return reply(&json!({"turn":null}), 200);
            }
            return reply(
                &json!({"turn":{"service_id":self.turn_service_id()?,"enabled":enabled(&db)?}}),
                200,
            );
        }
        if req.method() != Method::Post {
            return reply(&json!({"error":"method_not_allowed"}), 405);
        }
        if req
            .headers()
            .get("Content-Type")?
            .is_none_or(|value| value.split(';').next().map(str::trim) != Some("application/json"))
        {
            return reply(&json!({"error":"json_required"}), 415);
        }
        if req
            .headers()
            .get("Content-Length")?
            .and_then(|value| value.parse::<usize>().ok())
            .is_some_and(|size| size > MAX_BODY_BYTES)
        {
            return reply(&json!({"error":"body_too_large"}), 413);
        }
        let bytes = req.bytes().await?;
        if bytes.len() > MAX_BODY_BYTES {
            return reply(&json!({"error":"body_too_large"}), 413);
        }
        let body: Value = match serde_json::from_slice(&bytes) {
            Ok(body) => body,
            Err(_) => return reply(&json!({"error":"invalid_request"}), 400),
        };
        // Body reads yield too. Resolve authority again before reserving capacity.
        let Some(current) =
            db.get::<Credential>(&format!("credential:{}", text_id(&credential.id)))?
        else {
            return reply(&json!({"error":"access_denied"}), 403);
        };
        credential = current;
        if !revoke && credential.state != CredentialState::Active {
            return reply(&json!({"error":"access_denied"}), 403);
        }
        let request: credentials::LeaseRequest = match serde_json::from_value(body) {
            Ok(value) => value,
            Err(_) => return reply(&json!({"error":"invalid_request"}), 400),
        };
        if !request.valid() || request.service_id != self.turn_service_id()? {
            return reply(&json!({"error":"invalid_request"}), 400);
        }
        db.cleanup(now)?;
        self.schedule().await?;
        if revoke {
            let response = Self::turn_revoke(&db, &credential, &request, io::now().0)?;
            self.schedule().await?;
            return Ok(response);
        }
        if !matches!(path.as_str(), "/api/turn/issue" | "/api/turn/renew") {
            return reply(&json!({"error":"not_found"}), 404);
        }
        self.turn_issue(&db, &credential, request, path.ends_with("/renew"))
            .await
    }
    pub(super) fn manage_turn(
        &self,
        db: &Database,
        req: &Request,
        body: &Value,
        now: u64,
    ) -> Result<Response> {
        match (req.method(), req.path().as_str()) {
            (Method::Get, "/api/turn/status") => self.turn_status(db, now),
            (Method::Post, "/api/turn/settings") => {
                let Some(value) = body.get("enabled").and_then(Value::as_bool) else {
                    return reply(&json!({"error":"invalid_request"}), 400);
                };
                db.put(ENABLED_KEY, &value, None)?;
                if !value {
                    Self::withdraw_turn(db, None, now)?;
                }
                trace(
                    "turn_service_configured",
                    &json!({"service_id":self.turn_service_id()?,"enabled":value}),
                );
                self.turn_status(db, now)
            }
            (Method::Get | Method::Post, "/api/turn/provision") => self.turn_provision(db, body),
            _ => reply(&json!({"error":"not_found"}), 404),
        }
    }
    fn turn_status(&self, db: &Database, now: u64) -> Result<Response> {
        let mut counts = serde_json::Map::new();
        for mut lease in db.list::<Lease>(LEASE_PREFIX)? {
            lease.settle_expiry(now);
            let count = counts.entry(lease.status().to_string()).or_insert(json!(0));
            *count = json!(count.as_u64().unwrap_or(0) + 1);
        }
        reply(
            &json!({"ready":self.turn_ready(db)?,"enabled":enabled(db)?,"leases":counts,"analytics_available":analytics::available(&self.env)}),
            200,
        )
    }
}
