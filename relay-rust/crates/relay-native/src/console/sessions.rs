use super::{Console, Digest, Rejected, Reply, Result, database, field, invalid, random};
use argon2::{
    Argon2,
    password_hash::{PasswordHash, PasswordHasher, PasswordVerifier, SaltString},
};
use hyper::StatusCode;
use rusqlite::{Connection, OptionalExtension, params};
use serde_json::{Value, json};
use windshare_relay_console::text_id;

const SESSION_LIFETIME_MS: u64 = 12 * 60 * 60 * 1000;
const MIN_PASSWORD: usize = 12;
const MAX_PASSWORD: usize = 256;
const MAX_LOGIN_ATTEMPTS: u32 = 12;

pub(super) fn initialized(db: &Connection) -> Result<bool> {
    Ok(
        db.query_row("SELECT EXISTS(SELECT 1 FROM console_owner)", [], |r| {
            r.get(0)
        })?,
    )
}

pub(super) fn authenticated(db: &Connection, session: &str, now: u64) -> Result<bool> {
    if session.is_empty() {
        return Ok(false);
    }
    Ok(db.query_row(
        "SELECT EXISTS(SELECT 1 FROM console_sessions WHERE digest=?1 AND expires>?2)",
        params![
            Digest::of(session.as_bytes()).0.as_slice(),
            i64::try_from(now)?
        ],
        |r| r.get(0),
    )?)
}

fn sign_in(db: &Connection, now: u64) -> Result<Reply> {
    let secret = text_id(&random::<32>()?);
    db.execute(
        "INSERT INTO console_sessions VALUES(?1,?2)",
        params![
            Digest::of(secret.as_bytes()).0.as_slice(),
            i64::try_from(now + SESSION_LIFETIME_MS)?
        ],
    )?;
    Ok(Reply {
        cookie: Some(secret),
        ..Reply::ok(json!({"ok":true}))
    })
}

fn hash_password(password: &str) -> Result<String> {
    if !(MIN_PASSWORD..=MAX_PASSWORD).contains(&password.len()) {
        return Err(invalid().into());
    }
    let salt = SaltString::encode_b64(&random::<16>()?).map_err(|_| invalid())?;
    Ok(Argon2::default()
        .hash_password(password.as_bytes(), &salt)
        .map_err(|_| invalid())?
        .to_string())
}

fn verify_stored_password(db: &Connection, password: &str) -> Result<bool> {
    let stored: Option<String> = db
        .query_row("SELECT password FROM console_owner WHERE id=1", [], |r| {
            r.get(0)
        })
        .optional()?;
    Ok(password.len() <= MAX_PASSWORD
        && stored.as_ref().is_some_and(|s| {
            PasswordHash::new(s).is_ok_and(|hash| {
                Argon2::default()
                    .verify_password(password.as_bytes(), &hash)
                    .is_ok()
            })
        }))
}

impl Console {
    pub(super) fn initialize(&self, db: &Connection, body: &Value, now: u64) -> Result<Reply> {
        if initialized(db)? || Digest::of(field(body, "token")?.as_bytes()) != self.0.bootstrap {
            return Err(Rejected(StatusCode::FORBIDDEN, "initialization_consumed", 0).into());
        }
        let hash = hash_password(field(body, "password")?)?;
        db.execute("INSERT INTO console_owner VALUES(1,?1)", [hash])?;
        tracing::info!(event = "node_administrator_initialized");
        sign_in(db, now)
    }

    pub(super) fn login(db: &Connection, body: &Value, now: u64) -> Result<Reply> {
        database::throttle(db, "login", MAX_LOGIN_ATTEMPTS, now)?;
        if !verify_stored_password(db, field(body, "password")?)? {
            return Err(Rejected(StatusCode::UNAUTHORIZED, "invalid_password", 0).into());
        }
        sign_in(db, now)
    }

    pub(super) fn change_password(db: &Connection, body: &Value, now: u64) -> Result<Reply> {
        database::throttle(db, "password_change", MAX_LOGIN_ATTEMPTS, now)?;
        let current = field(body, "current_password")?;
        let next = field(body, "new_password")?;
        if !(MIN_PASSWORD..=MAX_PASSWORD).contains(&next.len()) {
            return Err(invalid().into());
        }
        if !verify_stored_password(db, current)? {
            return Err(Rejected(StatusCode::FORBIDDEN, "invalid_password", 0).into());
        }
        let hash = hash_password(next)?;
        db.execute("UPDATE console_owner SET password=?1 WHERE id=1", [hash])?;
        let invalidated = db.execute("DELETE FROM console_sessions", [])?;
        tracing::info!(
            event = "node_administrator_password_changed",
            invalidated_sessions = invalidated
        );
        sign_in(db, now)
    }

    pub(super) fn logout(db: &Connection, session: &str) -> Result<Reply> {
        db.execute(
            "DELETE FROM console_sessions WHERE digest=?1",
            [Digest::of(session.as_bytes()).0.as_slice()],
        )?;
        Ok(Reply {
            cookie: Some(String::new()),
            ..Reply::ok(json!({"ok":true}))
        })
    }
}
