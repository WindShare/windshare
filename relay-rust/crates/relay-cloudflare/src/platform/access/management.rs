use super::{
    AccessObject, Database, Digest, Refusal, key, parse_id, random, refusal, reply, required,
    text_id, trace,
};
use serde_json::{Value, json};
use windshare_relay_access::{Credential, CredentialState, Enrollment, EnrollmentState};
use windshare_relay_console::{
    MAX_ACTIVE_CREDENTIALS, credential_view,
    credentials::{RevokedCredentialPosition, credential_history},
    service_configuration,
};
use worker::{Method, Request, Response, Result};

impl AccessObject {
    pub(super) fn principal(db: &Database) -> Result<[u8; 16]> {
        if let Some(principal) = db.get("principal:owner")? {
            return Ok(principal);
        }
        let principal: [u8; 16] = random()?;
        db.put("principal:owner", &principal, None)?;
        Ok(principal)
    }
    pub(super) fn manage(
        &self,
        db: &Database,
        req: &Request,
        body: &Value,
        now: u64,
    ) -> Result<Response> {
        match (req.method(), req.path().as_str()) {
            (Method::Get, "/api/session") => {
                reply(&json!({"authenticated":true,"capabilities":{}}), 200)
            }
            (Method::Post, "/api/logout") => Self::logout(db, req),
            (Method::Get, "/api/credentials") => {
                let credentials: Vec<_> = db
                    .list_active_credentials()?
                    .iter()
                    .map(credential_view)
                    .collect();
                reply(&json!({"credentials":credentials}), 200)
            }
            (Method::Get, "/api/credentials/revoked") => {
                let cursor = req
                    .url()?
                    .query_pairs()
                    .find_map(|(name, value)| (name == "after").then(|| value.into_owned()));
                let after = match cursor.as_deref() {
                    None => None,
                    Some(cursor) => match RevokedCredentialPosition::parse(cursor) {
                        Some(position) => Some(position),
                        None => return reply(&json!({"error":"invalid_cursor"}), 400),
                    },
                };
                let page = credential_history(db.revoked_credentials(after.as_ref())?);
                trace(
                    "credential_history_listed",
                    &json!({"continued":after.is_some(),"has_more":!page["next_cursor"].is_null()}),
                );
                reply(&page, 200)
            }
            (Method::Post, "/api/credentials") => self.create_credential(db, body, now),
            (Method::Post, "/api/credentials/revoke") => Self::revoke(db, body, now),
            (Method::Get, "/api/enrollments") => {
                let enrollments: Vec<_> = db.list::<Enrollment>("enrollment:")?.iter()
                    .filter(|e| e.state == EnrollmentState::Pending).map(|e| json!({
                        "enrollment_id":text_id(&e.id),"user_code":e.user_code,"device_name":e.device_name,
                        "created_at":e.created_at,"expires_at":e.expires_at,
                    })).collect();
                reply(&json!({"enrollments":enrollments}), 200)
            }
            (Method::Post, "/api/enrollments/approve") => Self::approve(db, body, now),
            _ => reply(&json!({"error":"not_found"}), 404),
        }
    }
    fn create_credential(&self, db: &Database, body: &Value, now: u64) -> Result<Response> {
        let name = required(body, "name")?;
        if name.trim().is_empty() || name.len() > 128 || name.chars().any(char::is_control) {
            return refusal(Refusal::InvalidName);
        }
        if db.active_credentials()? >= MAX_ACTIVE_CREDENTIALS {
            return reply(&json!({"error":"credential_capacity"}), 429);
        }
        let access_key = format!("wsak_{}", text_id(&random::<32>()?));
        let credential = Credential {
            id: random()?,
            principal: random()?,
            name: name.to_string(),
            digest: Digest::of(access_key.as_bytes()),
            created_at: now,
            last_used_at: None,
            state: CredentialState::Active,
        };
        db.save_credential(&credential)?;
        trace(
            "credential_created",
            &json!({"credential_id":text_id(&credential.id),"principal_id":text_id(&credential.principal)}),
        );
        let relay = self.env.var("RELAY_URL")?.to_string();
        let configuration = service_configuration(&relay, &access_key);
        reply(
            &json!({"credential":credential_view(&credential),"access_key":access_key,"configuration":configuration}),
            201,
        )
    }
    fn approve(db: &Database, body: &Value, now: u64) -> Result<Response> {
        let id: [u8; 16] = parse_id(required(body, "enrollment_id")?)?;
        let Some(mut enrollment) = db.get::<Enrollment>(&key("enrollment", &id))? else {
            return refusal(Refusal::Expired);
        };
        // The administrator confirms the code displayed by the initiating device.
        if required(body, "user_code")? != enrollment.user_code {
            return refusal(Refusal::AccessDenied);
        }
        if db.active_credentials()? >= MAX_ACTIVE_CREDENTIALS {
            return reply(&json!({"error":"credential_capacity"}), 429);
        }
        if let Err(error) = enrollment.approve(random()?, now) {
            return refusal(error);
        }
        db.put(
            &key("enrollment", &id),
            &enrollment,
            Some(enrollment.expires_at),
        )?;
        trace(
            "enrollment_approved",
            &json!({"enrollment_id":text_id(&id),"principal_id":text_id(&Self::principal(db)?)}),
        );
        reply(&json!({"ok":true}), 200)
    }
    fn revoke(db: &Database, body: &Value, now: u64) -> Result<Response> {
        let id: [u8; 16] = parse_id(required(body, "credential_id")?)?;
        let Some(mut credential) = db.get::<Credential>(&key("credential", &id))? else {
            return reply(&json!({"error":"not_found"}), 404);
        };
        credential.revoke(now);
        db.save_credential(&credential)?;
        let terminate = body
            .get("terminate_shares")
            .and_then(Value::as_bool)
            .unwrap_or(false);
        if terminate {
            db.queue_termination(&text_id(&id))?;
        }
        trace(
            "credential_revoked",
            &json!({"credential_id":text_id(&id),"principal_id":text_id(&credential.principal),"terminate_shares":terminate}),
        );
        reply(&json!({"ok":true,"termination_pending":terminate}), 200)
    }
    pub(super) fn authorize(db: &Database, body: &Value, now: u64) -> Result<Response> {
        let digest = Digest(parse_id(required(body, "digest")?)?);
        let share = required(body, "share_id")?;
        let _: [u8; 12] = parse_id(share)?;
        let Some(mut credential) = db.credential(digest)? else {
            trace(
                "registration_refused",
                &json!({"share_id":share,"operation_id":body.get("operation_id"),"reason":"unknown_credential"}),
            );
            return refusal(Refusal::AccessDenied);
        };
        let attribution = match credential.authorize(digest, now) {
            Ok(value) => value,
            Err(error) => {
                trace(
                    "registration_refused",
                    &json!({"share_id":share,"operation_id":body.get("operation_id"),
                    "credential_id":text_id(&credential.id),"principal_id":text_id(&credential.principal),"reason":"revoked_credential"}),
                );
                return refusal(error);
            }
        };
        db.save_credential(&credential)?;
        db.track_share(&text_id(&credential.id), share)?;
        trace(
            "registration_authorized",
            &json!({"credential_id":text_id(&credential.id),"principal_id":text_id(&credential.principal),"share_id":share,
            "operation_id":body.get("operation_id")}),
        );
        reply(&json!({"attribution":attribution}), 200)
    }
}
