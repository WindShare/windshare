use super::{
    AccessObject, COOKIE_NAME, Database, MAX_RECORDS, failure, key, random, reply, reply_error,
    trace,
};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use windshare_relay_access::Digest;
use windshare_relay_console::{
    ADMIN_SESSION_LIFETIME_MS,
    http::{ConsoleError, extract_cookie, format_session_cookie, required_str},
    text_id,
};
use worker::{Request, Response, Result};

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
        let name = format!("attempt:{scope}");
        let mut attempts = db
            .get::<Attempts>(&name)?
            .filter(|a| a.expires_at > now)
            .unwrap_or_else(|| Attempts {
                count: 0,
                expires_at: now.saturating_add(ATTEMPT_WINDOW_MS),
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
        let Ok(password) = required_str(body, "password") else {
            return reply_error(ConsoleError::bad_request("invalid_request"));
        };
        let source = req.headers().get("CF-Connecting-IP")?.unwrap_or_default();
        if !Self::rate_limit(db, "login:global", GLOBAL_LOGIN_ATTEMPTS, now)?
            || !Self::rate_limit(
                db,
                &format!("login:{}", text_id(&Digest::of(source.as_bytes()).0)),
                LOGIN_ATTEMPTS,
                now,
            )?
        {
            return reply_error(ConsoleError::too_many_requests(
                "slow_down",
                ATTEMPT_WINDOW_MS,
            ));
        }
        let revision = self.password_revision()?;
        let supplied = Digest::of(password.as_bytes());
        let different = revision
            .0
            .iter()
            .zip(supplied.0)
            .fold(0_u8, |diff, (a, b)| diff | (a ^ b));
        if different != 0 {
            return reply_error(ConsoleError::unauthorized("invalid_password"));
        }
        if db.list::<Session>("session:")?.len() >= MAX_RECORDS {
            return reply_error(ConsoleError::too_many_requests("session_capacity", 0));
        }
        let token = text_id(&random::<32>()?);
        let session = Session {
            password_revision: revision,
            expires_at: now.saturating_add(ADMIN_SESSION_LIFETIME_MS),
        };
        db.put(
            &key("session", &Digest::of(token.as_bytes()).0),
            &session,
            Some(session.expires_at),
        )?;
        trace(
            "administrator_signed_in",
            &json!({"session_id": text_id(&Digest::of(token.as_bytes()).0)}),
        );
        let mut response = reply(&json!({"ok": true}), 200)?;
        response.headers_mut().set(
            "Set-Cookie",
            &format_session_cookie(COOKIE_NAME, &token, "/", true),
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
        let mut response = reply(&json!({"ok": true}), 200)?;
        response.headers_mut().set(
            "Set-Cookie",
            &format_session_cookie(COOKIE_NAME, "", "/", true),
        )?;
        Ok(response)
    }
}

fn cookie(req: &Request) -> Result<Option<String>> {
    Ok(req
        .headers()
        .get("Cookie")?
        .and_then(|header| extract_cookie(&header, COOKIE_NAME).map(str::to_owned)))
}
