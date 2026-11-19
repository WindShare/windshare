use super::{Console, Digest, Rejected, Reply, Result, database, field, id, random, refusal};
use hyper::{Method, StatusCode};
use rusqlite::{Connection, OptionalExtension};
use serde_json::{Value, json};
use windshare_relay_access::{
    Credential, CredentialState, Enrollment, EnrollmentState, Poll, Refusal,
};
use windshare_relay_console::{
    MAX_ACTIVE_CREDENTIALS, MAX_RECORDS, claimed_key, credential_view, enrollment_response,
    service_configuration, text_id, user_code,
};

pub(super) fn require_credential_capacity(db: &Connection) -> Result<()> {
    if database::active_credentials(db)? >= MAX_ACTIVE_CREDENTIALS {
        return Err(Rejected(StatusCode::TOO_MANY_REQUESTS, "credential_capacity", 0).into());
    }
    Ok(())
}

fn issue(id: [u8; 16], principal: [u8; 16], name: String, key: &str, now: u64) -> Credential {
    Credential {
        id,
        principal,
        name,
        digest: Digest::of(key.as_bytes()),
        created_at: now,
        last_used_at: None,
        state: CredentialState::Active,
    }
}

impl Console {
    fn enroll(&self, db: &Connection, body: &Value, now: u64) -> Result<Reply> {
        let digest = Digest(id(body, "claim_digest")?);
        let existing: Option<String> = db
            .query_row(
                "SELECT body FROM console_enrollments WHERE claimant=?1",
                [digest.0.as_slice()],
                |r| r.get(0),
            )
            .optional()?;
        let e = if let Some(s) = existing {
            serde_json::from_str(&s)?
        } else {
            database::throttle(db, "enroll", 60, now)?;
            let count: u32 = db.query_row(
                "SELECT count(*) FROM console_enrollments WHERE expires IS NOT NULL",
                [],
                |r| r.get(0),
            )?;
            if usize::try_from(count)? >= MAX_RECORDS {
                return Err(Rejected(StatusCode::TOO_MANY_REQUESTS, "slow_down", 60_000).into());
            }
            let e = Enrollment::new(
                random()?,
                user_code(random()?),
                field(body, "device_name")?.to_owned(),
                digest,
                now,
            )
            .map_err(refusal)?;
            database::save_enrollment(db, &e)?;
            tracing::info!(event="enrollment_requested",enrollment_id=%text_id(&e.id), device_name=%e.device_name);
            e
        };
        let mut response = enrollment_response(&e, self.origin());
        if let Some(token) = self.account_invitation(&e, now)? {
            response["account_invitation"] = token.into();
        }
        Ok(Reply::ok(response))
    }

    pub(super) fn device(
        &self,
        db: &Connection,
        path: &str,
        body: &Value,
        now: u64,
    ) -> Result<Reply> {
        if path == "/api/enrollments" {
            return self.enroll(db, body, now);
        }
        let eid = id(body, "enrollment_id")?;
        let secret = id::<32>(body, "claim_secret")?;
        let Some(mut e) = database::enrollment(db, &eid)? else {
            if path == "/api/enrollments/cancel" {
                return Ok(Reply::ok(json!({"ok":true})));
            }
            return Err(refusal(Refusal::Expired).into());
        };
        let claimant = Digest::of(&secret);
        let key = claimed_key(&secret);
        let reply = match path {
            "/api/enrollments/claim" => match e.poll(claimant, now).map_err(refusal)? {
                Poll::Pending => {
                    database::save_enrollment(db, &e)?;
                    return Err(refusal(Refusal::Pending).into());
                }
                Poll::Approved { credential } | Poll::Installed { credential } => {
                    if self
                        .0
                        .access
                        .credential_inventory()
                        .iter()
                        .any(|c| c.id == credential && c.state != CredentialState::Active)
                    {
                        return Err(refusal(Refusal::AccessDenied).into());
                    }
                    json!({"access_key":key,"credential_id":text_id(&credential)})
                }
            },
            "/api/enrollments/installed" => {
                let delivered = matches!(e.state, EnrollmentState::Installed { .. });
                e.installed(claimant, now).map_err(refusal)?;
                if let EnrollmentState::Installed { credential } = e.state {
                    if delivered {
                        if !self
                            .0
                            .access
                            .credential_inventory()
                            .iter()
                            .any(|c| c.id == credential && c.state == CredentialState::Active)
                        {
                            return Err(refusal(Refusal::AccessDenied).into());
                        }
                    } else {
                        require_credential_capacity(db)?;
                        database::save_key(
                            db,
                            &issue(
                                credential,
                                database::binding(db, &credential)?
                                    .map_or(self.0.owner, |binding| binding.account),
                                e.device_name.clone(),
                                &key,
                                now,
                            ),
                        )?;
                        tracing::info!(event="enrollment_installed",enrollment_id=%text_id(&eid),credential_id=%text_id(&credential));
                    }
                }
                json!({"ok":true,"managed_turn":[]})
            }
            "/api/enrollments/cancel" => {
                if let Some(id) = e.cancel(claimant).map_err(refusal)? {
                    self.revoke(db, id, now)?;
                }
                json!({"ok":true})
            }
            _ => return Err(super::invalid().into()),
        };
        database::save_enrollment(db, &e)?;
        Ok(Reply::ok(reply))
    }

    fn revoke(&self, db: &Connection, id: [u8; 16], now: u64) -> Result<bool> {
        let Some(mut c) = self
            .0
            .access
            .credential_inventory()
            .into_iter()
            .find(|c| c.id == id)
        else {
            return Ok(false);
        };
        c.revoke(now);
        database::save_key(db, &c)?;
        tracing::info!(event="node_credential_revoked",credential_id=%text_id(&id),principal_id=%text_id(&c.principal));
        Ok(true)
    }

    pub(super) fn manage(
        &self,
        db: &Connection,
        method: &Method,
        path: &str,
        body: &Value,
        now: u64,
    ) -> Result<Reply> {
        let value = match (method, path) {
            (&Method::GET, "/api/enrollments") => {
                let mut statement = db.prepare(
                    "SELECT body FROM console_enrollments WHERE expires IS NOT NULL ORDER BY rowid",
                )?;
                let mut requests = Vec::new();
                for s in statement.query_map([], |r| r.get::<_, String>(0))? {
                    let e: Enrollment = serde_json::from_str(&s?)?;
                    if e.state == EnrollmentState::Pending {
                        requests.push(json!({"enrollment_id":text_id(&e.id),"user_code":e.user_code,"device_name":e.device_name,"expires_at":e.expires_at}));
                    }
                }
                json!({"enrollments":requests})
            }
            (&Method::POST, "/api/enrollments/approve") => {
                let mut e = database::enrollment(db, &id(body, "enrollment_id")?)?
                    .ok_or_else(|| refusal(Refusal::Expired))?;
                if field(body, "user_code")? != e.user_code {
                    return Err(refusal(Refusal::AccessDenied).into());
                }
                require_credential_capacity(db)?;
                e.approve(random()?, now).map_err(refusal)?;
                database::save_enrollment(db, &e)?;
                tracing::info!(event="enrollment_approved",enrollment_id=%text_id(&e.id));
                json!({"ok":true})
            }
            (&Method::GET, "/api/credentials") => {
                json!({"credentials":self.0.access.credential_inventory().iter().map(credential_view).collect::<Vec<_>>()})
            }
            (&Method::POST, "/api/credentials") => {
                let name = field(body, "name")?;
                if name.trim().is_empty() || name.len() > 128 || name.chars().any(char::is_control)
                {
                    return Err(refusal(Refusal::InvalidName).into());
                }
                require_credential_capacity(db)?;
                let key = format!("wsak_{}", text_id(&random::<32>()?));
                let c = issue(random()?, random()?, name.to_owned(), &key, now);
                database::save_key(db, &c)?;
                tracing::info!(event="service_key_created",credential_id=%text_id(&c.id));
                return Ok(Reply {
                    status: StatusCode::CREATED,
                    ..Reply::ok(
                        json!({"credential":credential_view(&c),"access_key":key,"configuration":service_configuration(&self.0.relay,&key)}),
                    )
                });
            }
            (&Method::POST, "/api/credentials/revoke") => {
                if !self.revoke(db, id(body, "credential_id")?, now)? {
                    return Err(Rejected(StatusCode::NOT_FOUND, "not_found", 0).into());
                }
                json!({"ok":true})
            }
            _ => return Err(Rejected(StatusCode::NOT_FOUND, "not_found", 0).into()),
        };
        Ok(Reply::ok(value))
    }
}
