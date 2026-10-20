use super::{
    AccessObject, Database, Digest, MAX_RECORDS, Refusal, key, parse_id, random, refusal, reply,
    required, text_id, trace,
};
use serde_json::{Value, json};
use windshare_relay_access::{
    Credential, CredentialState, Enrollment, EnrollmentState, POLL_INTERVAL_MS, Poll,
};
use worker::{Request, Response, Result};
const CODE_ALPHABET: &[u8; 32] = b"ABCDEFGHJKLMNPQRSTUVWXYZ23456789";
const KEY_DOMAIN: &[u8] = b"windshare/access-key/v1\0";
const ENROLLMENT_ATTEMPTS: u32 = 30;
const GLOBAL_ENROLLMENT_ATTEMPTS: u32 = 1000;

impl AccessObject {
    pub(super) fn enroll(
        &self,
        db: &Database,
        req: &Request,
        body: &Value,
        now: u64,
    ) -> Result<Response> {
        let claim_digest = Digest(parse_id(required(body, "claim_digest")?)?);
        if let Some(id) = db.get::<[u8; 16]>(&key("request", &claim_digest.0))?
            && let Some(enrollment) = db.get::<Enrollment>(&key("enrollment", &id))?
        {
            return self.enrollment_response(&enrollment);
        }
        let source = req.headers().get("CF-Connecting-IP")?.unwrap_or_default();
        if !Self::rate_limit(db, "enroll:global", GLOBAL_ENROLLMENT_ATTEMPTS, now)?
            || !Self::rate_limit(
                db,
                &format!("enroll:{}", text_id(&Digest::of(source.as_bytes()).0)),
                ENROLLMENT_ATTEMPTS,
                now,
            )?
            || db.list::<Enrollment>("enrollment:")?.len() >= MAX_RECORDS
        {
            return reply(&json!({"error":"slow_down","retry_after_ms":60_000}), 429);
        }
        let entropy = random::<8>()?;
        let code: String = entropy
            .iter()
            .enumerate()
            .flat_map(|(index, byte)| {
                let character = CODE_ALPHABET[usize::from(byte & 31)] as char;
                if index == 4 {
                    vec!['-', character]
                } else {
                    vec![character]
                }
            })
            .collect();
        let enrollment = match Enrollment::new(
            random()?,
            code,
            required(body, "device_name")?.to_string(),
            claim_digest,
            now,
        ) {
            Ok(enrollment) => enrollment,
            Err(error) => return refusal(error),
        };
        db.put(
            &key("enrollment", &enrollment.id),
            &enrollment,
            Some(enrollment.expires_at),
        )?;
        db.put(
            &key("request", &claim_digest.0),
            &enrollment.id,
            Some(enrollment.expires_at),
        )?;
        trace(
            "enrollment_requested",
            &json!({"enrollment_id":text_id(&enrollment.id),"device_name":enrollment.device_name}),
        );
        self.enrollment_response(&enrollment)
    }
    fn enrollment_response(&self, enrollment: &Enrollment) -> Result<Response> {
        reply(
            &json!({"enrollment_id":text_id(&enrollment.id),"user_code":enrollment.user_code,
            "verification_uri":format!("{}/",self.console_origin()?),
            "verification_uri_complete":format!("{}/?code={}",self.console_origin()?,enrollment.user_code),
            "expires_at":enrollment.expires_at,"interval_ms":POLL_INTERVAL_MS}),
            200,
        )
    }
    pub(super) fn claim(db: &Database, body: &Value, now: u64) -> Result<Response> {
        let Some(mut enrollment) = load(db, body)? else {
            return refusal(Refusal::Expired);
        };
        let secret: [u8; 32] = parse_id(required(body, "claim_secret")?)?;
        let result = match enrollment.poll(Digest::of(&secret), now) {
            Ok(result) => result,
            Err(error) => return refusal(error),
        };
        db.put(
            &key("enrollment", &enrollment.id),
            &enrollment,
            Some(enrollment.expires_at),
        )?;
        if matches!(enrollment.state, EnrollmentState::Installed { .. }) {
            db.put(&key("enrollment", &enrollment.id), &enrollment, None)?;
        }
        match result {
            Poll::Pending => refusal(Refusal::Pending),
            Poll::Approved { credential } | Poll::Installed { credential } => {
                // Derivation lets a lost response recover exactly the same credential;
                // only digests and public identifiers survive in server storage.
                let material = [KEY_DOMAIN, &secret].concat();
                let access_key = format!("wsak_{}", text_id(&Digest::of(&material).0));
                let saved = db.get::<Credential>(&key("credential", &credential))?;
                if let Some(saved) = &saved {
                    if saved.state != CredentialState::Active {
                        return refusal(Refusal::AccessDenied);
                    }
                } else if matches!(enrollment.state, EnrollmentState::Installed { .. }) {
                    return refusal(Refusal::AccessDenied);
                } else {
                    let principal = Self::principal(db)?;
                    db.save_credential(&Credential {
                        id: credential,
                        principal,
                        name: enrollment.device_name.clone(),
                        digest: Digest::of(access_key.as_bytes()),
                        created_at: now,
                        last_used_at: None,
                        state: CredentialState::Active,
                    })?;
                }
                trace(
                    "enrollment_claimed",
                    &json!({"enrollment_id":text_id(&enrollment.id),"credential_id":text_id(&credential)}),
                );
                reply(
                    &json!({"access_key":access_key,"credential_id":text_id(&credential),
                    "expires_at":enrollment.expires_at}),
                    200,
                )
            }
        }
    }
    pub(super) fn installed(db: &Database, body: &Value, now: u64) -> Result<Response> {
        let Some(mut enrollment) = load(db, body)? else {
            return refusal(Refusal::Expired);
        };
        let secret: [u8; 32] = parse_id(required(body, "claim_secret")?)?;
        if let Err(error) = enrollment.installed(Digest::of(&secret), now) {
            return refusal(error);
        }
        // Keep the installation receipt while its credential exists so a failed
        // final client write can safely retry even after the approval window.
        db.put(&key("enrollment", &enrollment.id), &enrollment, None)?;
        db.put(
            &key("request", &enrollment.claim_digest.0),
            &enrollment.id,
            None,
        )?;
        trace(
            "enrollment_installed",
            &json!({"enrollment_id":text_id(&enrollment.id)}),
        );
        reply(&json!({"ok":true}), 200)
    }
    pub(super) fn cancel(db: &Database, body: &Value, _now: u64) -> Result<Response> {
        let Some(mut enrollment) = load(db, body)? else {
            return reply(&json!({"ok":true}), 200);
        };
        let secret: [u8; 32] = parse_id(required(body, "claim_secret")?)?;
        let credential = match enrollment.cancel(Digest::of(&secret)) {
            Ok(id) => id,
            Err(error) => return refusal(error),
        };
        if let Some(id) = credential {
            db.remove_credential(id)?;
            db.queue_termination(&text_id(&id))?;
        }
        db.put(
            &key("enrollment", &enrollment.id),
            &enrollment,
            Some(enrollment.expires_at),
        )?;
        trace(
            "enrollment_cancelled",
            &json!({"enrollment_id":text_id(&enrollment.id)}),
        );
        reply(&json!({"ok":true}), 200)
    }
}
fn load(db: &Database, body: &Value) -> Result<Option<Enrollment>> {
    db.get(&key(
        "enrollment",
        &parse_id::<16>(required(body, "enrollment_id")?)?,
    ))
}
