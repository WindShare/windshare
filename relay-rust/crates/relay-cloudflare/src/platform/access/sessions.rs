use super::{
    AccessObject, COOKIE_NAME, Database, Digest, MAX_RECORDS, failure, key, random, reply,
    required, text_id, trace,
};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use worker::{Request, Response, Result};
const SESSION_LIFETIME_MS: u64 = 8 * 60 * 60 * 1_000;
const ATTEMPT_WINDOW_MS: u64 = 15 * 60 * 1_000;
const LOGIN_ATTEMPTS: u32 = 8;
const GLOBAL_LOGIN_ATTEMPTS: u32 = 100;

#[derive(Serialize, Deserialize)]
struct Session {
    password_revision: Digest,
    expires_at: u64,
}
#[derive(Serialize, Deserialize)]
struct Attempts {
    count: u32,
    expires_at: u64,
}

impl AccessObject {
    fn password_revision(&self) -> Result<Digest> {
        let password = self.env.secret("OWNER_PASSWORD")?.to_string();
        if password.is_empty() {
            return Err(failure("owner password is not configured"));
        }
        Ok(Digest::of(password.as_bytes()))
    }
    pub(super) fn rate_limit(db: &Database, scope: &str, limit: u32, now: u64) -> Result<bool> {
        Self::rate_limit_window(db, scope, limit, now, ATTEMPT_WINDOW_MS)
    }
    pub(super) fn rate_limit_window(
        db: &Database,
        scope: &str,
        limit: u32,
        now: u64,
        window_ms: u64,
    ) -> Result<bool> {
        let name = format!("attempt:{scope}");
        let mut attempts = db
            .get::<Attempts>(&name)?
            .filter(|a| a.expires_at > now)
            .unwrap_or_else(|| Attempts {
                count: 0,
                expires_at: now.saturating_add(window_ms),
            });
        if attempts.count >= limit {
            return Ok(false);
        }
        attempts.count += 1;
        db.put(&name, &attempts, Some(attempts.expires_at))?;
        Ok(true)
    }
    pub(super) fn login(
        &self,
        db: &Database,
        req: &Request,
        body: &Value,
        now: u64,
    ) -> Result<Response> {
        if req.headers().get("Origin")?.as_deref() != Some(self.console_origin()?.as_str()) {
            return reply(&json!({"error":"origin_required"}), 403);
        }
        let source = req.headers().get("CF-Connecting-IP")?.unwrap_or_default();
        if !Self::rate_limit(db, "login:global", GLOBAL_LOGIN_ATTEMPTS, now)?
            || !Self::rate_limit(
                db,
                &format!("login:{}", text_id(&Digest::of(source.as_bytes()).0)),
                LOGIN_ATTEMPTS,
                now,
            )?
        {
            return reply(
                &json!({"error":"slow_down","retry_after_ms":ATTEMPT_WINDOW_MS}),
                429,
            );
        }
        let revision = self.password_revision()?;
        let supplied = Digest::of(required(body, "password")?.as_bytes());
        let different = revision
            .0
            .iter()
            .zip(supplied.0)
            .fold(0_u8, |diff, (a, b)| diff | (a ^ b));
        if different != 0 {
            return reply(&json!({"error":"invalid_password"}), 401);
        }
        if db.list::<Session>("session:")?.len() >= MAX_RECORDS {
            return reply(&json!({"error":"session_capacity"}), 429);
        }
        let token = text_id(&random::<32>()?);
        let session = Session {
            password_revision: revision,
            expires_at: now.saturating_add(SESSION_LIFETIME_MS),
        };
        db.put(
            &key("session", &Digest::of(token.as_bytes()).0),
            &session,
            Some(session.expires_at),
        )?;
        trace(
            "administrator_signed_in",
            &json!({"session_id":text_id(&Digest::of(token.as_bytes()).0)}),
        );
        let mut response = reply(&json!({"ok":true}), 200)?;
        response.headers_mut().set(
            "Set-Cookie",
            &format!(
                "{COOKIE_NAME}={token}; Path=/; HttpOnly; Secure; SameSite=Strict; Max-Age={}",
                SESSION_LIFETIME_MS / 1_000
            ),
        )?;
        Ok(response)
    }
    pub(super) fn authenticated(&self, db: &Database, req: &Request, now: u64) -> Result<bool> {
        let Some(token) = cookie(req)? else {
            return Ok(false);
        };
        let Some(session) = db.get::<Session>(&key("session", &Digest::of(token.as_bytes()).0))?
        else {
            return Ok(false);
        };
        Ok(session.expires_at > now && session.password_revision == self.password_revision()?)
    }
    pub(super) fn logout(db: &Database, req: &Request) -> Result<Response> {
        if let Some(token) = cookie(req)? {
            db.delete(&key("session", &Digest::of(token.as_bytes()).0))?;
        }
        let mut response = reply(&json!({"ok":true}), 200)?;
        response.headers_mut().set(
            "Set-Cookie",
            &format!("{COOKIE_NAME}=; Path=/; HttpOnly; Secure; SameSite=Strict; Max-Age=0"),
        )?;
        Ok(response)
    }
}
fn cookie(req: &Request) -> Result<Option<String>> {
    Ok(req.headers().get("Cookie")?.and_then(|header| {
        header.split(';').find_map(|part| {
            let (name, value) = part.trim().split_once('=')?;
            (name == COOKIE_NAME).then(|| value.to_string())
        })
    }))
}
