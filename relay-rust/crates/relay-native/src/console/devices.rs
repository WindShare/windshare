use super::{Console, Reply, Result, database};
use crate::node::access::Service;
use anyhow::anyhow;
use hyper::{Method, StatusCode};
use rusqlite::{Connection, OptionalExtension};
use serde_json::{Value, json};
use windshare_relay_access::{Credential, Digest, Enrollment};
use windshare_relay_console::{
    MAX_ACTIVE_CREDENTIALS,
    access::{self, AccessError, AccessStore},
    credentials::RevokedCredentialPosition,
    enrollment_response,
    http::ConsoleError,
    text_id,
};

const ENROLL_LIMIT_PER_MINUTE: u32 = 60;

pub(super) fn require_credential_capacity(db: &Connection) -> Result<()> {
    if database::active_credentials(db)? >= MAX_ACTIVE_CREDENTIALS {
        return Err(ConsoleError::too_many_requests("credential_capacity", 0).into());
    }
    Ok(())
}

fn into_anyhow<T>(result: std::result::Result<T, AccessError<anyhow::Error>>) -> Result<T> {
    match result {
        Ok(value) => Ok(value),
        Err(AccessError::Rejected(error)) => Err(error.into()),
        Err(AccessError::Store(error)) => Err(error),
    }
}

struct NativeAccessStore<'a, 'db> {
    tx: &'a mut database::Management<'db>,
    access: &'a Service,
    owner: [u8; 16],
}

impl AccessStore for NativeAccessStore<'_, '_> {
    type Error = anyhow::Error;

    fn fill_random(&mut self, dest: &mut [u8]) -> Result<()> {
        getrandom::fill(dest).map_err(|error| anyhow!("console entropy: {error}"))
    }

    fn check_enrollment_rate_limit(
        &mut self,
        now: u64,
    ) -> std::result::Result<(), AccessError<anyhow::Error>> {
        database::throttle(&self.tx.db, "enroll", ENROLL_LIMIT_PER_MINUTE, now).map_err(|error| {
            error
                .downcast::<ConsoleError>()
                .map_or_else(AccessError::Store, AccessError::Rejected)
        })
    }

    fn pending_enrollment_count(&self) -> Result<usize> {
        let count: u32 = self.tx.db.query_row(
            "SELECT count(*) FROM console_enrollments WHERE expires IS NOT NULL",
            [],
            |row| row.get(0),
        )?;
        Ok(usize::try_from(count)?)
    }

    fn enrollment_by_claimant(&self, claimant: Digest) -> Result<Option<Enrollment>> {
        let existing: Option<String> = self
            .tx
            .db
            .query_row(
                "SELECT body FROM console_enrollments WHERE claimant=?1",
                [claimant.0.as_slice()],
                |row| row.get(0),
            )
            .optional()?;
        existing
            .map(|body| serde_json::from_str(&body).map_err(anyhow::Error::from))
            .transpose()
    }

    fn enrollment(&self, id: &[u8; 16]) -> Result<Option<Enrollment>> {
        database::enrollment(&self.tx.db, id)
    }

    fn list_pending_enrollments(&self) -> Result<Vec<Enrollment>> {
        database::list_pending_enrollments(&self.tx.db)
    }

    fn save_enrollment(&mut self, enrollment: &Enrollment) -> Result<()> {
        database::save_enrollment(&self.tx.db, enrollment)
    }

    fn active_credential_count(&self) -> Result<usize> {
        database::active_credentials(&self.tx.db)
    }

    fn credential(&self, id: &[u8; 16]) -> Result<Option<Credential>> {
        Ok(self.access.credential_by_id(*id))
    }

    fn list_active_credentials(&self) -> Result<Vec<Credential>> {
        Ok(self.access.active_credentials())
    }

    fn list_revoked_credentials(
        &self,
        after: Option<&RevokedCredentialPosition>,
        limit: usize,
    ) -> Result<Vec<Credential>> {
        Ok(self.access.revoked_credentials(
            after.map(|position| (position.revoked_at(), position.credential_id())),
            limit,
        ))
    }

    fn principal_for_installed(&mut self, credential_id: &[u8; 16]) -> Result<[u8; 16]> {
        Ok(database::binding(&self.tx.db, credential_id)?
            .map_or(self.owner, |binding| binding.account))
    }

    fn save_credential(&mut self, credential: &Credential) -> Result<bool> {
        self.tx.save_credential(credential.clone())
    }

    fn cancel_installed_credential(&mut self, id: &[u8; 16], now: u64) -> Result<()> {
        let Some(mut key) = self.access.credential_by_id(*id) else {
            return Ok(());
        };
        key.revoke(now);
        let principal = key.principal;
        if self.tx.save_credential(key)? {
            tracing::info!(
                event = "node_credential_revoked",
                credential_id = %text_id(id),
                principal_id = %text_id(&principal)
            );
        }
        Ok(())
    }
}

impl Console {
    fn access_store<'a, 'db>(
        &'a self,
        tx: &'a mut database::Management<'db>,
    ) -> NativeAccessStore<'a, 'db> {
        NativeAccessStore {
            tx,
            access: &self.0.access,
            owner: self.0.owner,
        }
    }

    pub(super) fn device(
        &self,
        tx: &mut database::Management<'_>,
        path: &str,
        body: &Value,
        now: u64,
    ) -> Result<Reply> {
        let mut store = self.access_store(tx);
        let reply = match path {
            "/api/enrollments" => {
                let outcome = into_anyhow(access::enroll(&mut store, body, now))?;
                if outcome.created {
                    tracing::info!(
                        event = "enrollment_requested",
                        enrollment_id = %text_id(&outcome.enrollment.id),
                        device_name = %outcome.enrollment.device_name
                    );
                }
                let mut response = enrollment_response(&outcome.enrollment, self.origin());
                if let Some(token) = self.account_invitation(&outcome.enrollment, now)? {
                    response["account_invitation"] = token.into();
                }
                response
            }
            "/api/enrollments/claim" => {
                let mut outcome = into_anyhow(access::claim(&mut store, body, now))?;
                let bound = database::binding(&store.tx.db, &outcome.credential_id)?.is_some();
                outcome.body["role"] = if bound { "account" } else { "owner" }.into();
                outcome.body
            }
            "/api/enrollments/installed" => {
                let outcome = into_anyhow(access::installed(&mut store, body, now))?;
                if outcome.newly_installed {
                    tracing::info!(
                        event = "enrollment_installed",
                        enrollment_id = %text_id(&outcome.enrollment_id),
                        credential_id = %text_id(&outcome.credential_id)
                    );
                }
                json!({"ok": true})
            }
            "/api/enrollments/cancel" => {
                into_anyhow(access::cancel(&mut store, body, now))?;
                json!({"ok": true})
            }
            _ => return Err(ConsoleError::bad_request("invalid_request").into()),
        };
        Ok(Reply::ok(reply))
    }

    pub(super) fn manage(
        &self,
        tx: &mut database::Management<'_>,
        method: &Method,
        path: &str,
        query: &str,
        body: &Value,
        now: u64,
    ) -> Result<Reply> {
        let mut store = self.access_store(tx);
        let value = match (method, path) {
            (&Method::GET, "/api/enrollments") => {
                into_anyhow(access::list_pending_enrollments(&store))?
            }
            (&Method::POST, "/api/enrollments/approve") => {
                let outcome = into_anyhow(access::approve(&mut store, body, now))?;
                tracing::info!(
                    event = "enrollment_approved",
                    enrollment_id = %text_id(&outcome.enrollment_id)
                );
                json!({"ok": true})
            }
            (&Method::GET, "/api/credentials") => {
                into_anyhow(access::list_active_credentials(&store))?
            }
            (&Method::GET, "/api/credentials/revoked") => {
                let cursor = url::form_urlencoded::parse(query.as_bytes())
                    .find_map(|(name, value)| (name == "after").then_some(value));
                let history =
                    into_anyhow(access::list_revoked_credentials(&store, cursor.as_deref()))?;
                tracing::debug!(
                    event = "credential_history_listed",
                    continued = history.continued,
                    has_more = history.has_more
                );
                history.body
            }
            (&Method::POST, "/api/credentials") => {
                let created = into_anyhow(access::create_service_key(
                    &mut store,
                    body,
                    &self.0.relay,
                    now,
                ))?;
                tracing::info!(
                    event = "service_key_created",
                    credential_id = %text_id(&created.credential.id)
                );
                return Ok(Reply {
                    status: StatusCode::CREATED,
                    ..Reply::ok(created.body)
                });
            }
            (&Method::POST, "/api/credentials/revoke") => {
                let revoked = into_anyhow(access::revoke_credential(&mut store, body, now))?;
                if revoked.changed {
                    tracing::info!(
                        event = "node_credential_revoked",
                        credential_id = %text_id(&revoked.credential.id),
                        principal_id = %text_id(&revoked.credential.principal)
                    );
                }
                json!({"ok": true})
            }
            _ => return Err(ConsoleError::not_found().into()),
        };
        Ok(Reply::ok(value))
    }
}
