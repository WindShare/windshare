use super::{Rejected, Result};
use crate::node::access::DeviceKeys;
use hyper::StatusCode;
use rusqlite::{Connection, OptionalExtension, params};
use windshare_relay_access::{Credential, Enrollment, EnrollmentState};

pub(super) fn initialize(db: &Connection) -> Result<()> {
    db.execute_batch("CREATE TABLE IF NOT EXISTS console_owner (id INTEGER PRIMARY KEY CHECK(id=1), password TEXT NOT NULL) STRICT;
        CREATE TABLE IF NOT EXISTS console_sessions (digest BLOB PRIMARY KEY, expires INTEGER NOT NULL) STRICT;
        CREATE TABLE IF NOT EXISTS console_enrollments (id BLOB PRIMARY KEY, claimant BLOB UNIQUE NOT NULL, expires INTEGER, body TEXT NOT NULL) STRICT;
        CREATE TABLE IF NOT EXISTS console_credentials (id BLOB PRIMARY KEY, body TEXT NOT NULL) STRICT;
        CREATE TABLE IF NOT EXISTS console_attempts (scope TEXT PRIMARY KEY, expires INTEGER NOT NULL, attempts INTEGER NOT NULL) STRICT;")?;
    Ok(())
}

pub(super) fn cleanup(db: &Connection, now: u64) -> Result<()> {
    let now = i64::try_from(now)?;
    db.execute("DELETE FROM console_enrollments WHERE expires <= ?1", [now])?;
    db.execute("DELETE FROM console_sessions WHERE expires <= ?1", [now])?;
    db.execute("DELETE FROM console_attempts WHERE expires <= ?1", [now])?;
    Ok(())
}

pub(super) fn throttle(db: &Connection, scope: &str, limit: u32, now: u64) -> Result<()> {
    const WINDOW_MS: u64 = 60_000;
    let changed=db.execute("INSERT INTO console_attempts VALUES(?1,?2,1) ON CONFLICT(scope) DO UPDATE SET attempts=attempts+1 WHERE attempts<?3",
        params![scope,i64::try_from(now+WINDOW_MS)?,limit])?;
    if changed == 0 {
        return Err(Rejected(StatusCode::TOO_MANY_REQUESTS, "slow_down", WINDOW_MS).into());
    }
    Ok(())
}

pub(super) fn enrollment(db: &Connection, id: &[u8; 16]) -> Result<Option<Enrollment>> {
    let body: Option<String> = db
        .query_row(
            "SELECT body FROM console_enrollments WHERE id=?1",
            [id.as_slice()],
            |r| r.get(0),
        )
        .optional()?;
    Ok(body.map(|v| serde_json::from_str(&v)).transpose()?)
}

pub(super) fn save_enrollment(db: &Connection, e: &Enrollment) -> Result<()> {
    let expires = (!matches!(e.state, EnrollmentState::Installed { .. }))
        .then_some(i64::try_from(e.expires_at)?);
    db.execute("INSERT INTO console_enrollments VALUES(?1,?2,?3,?4) ON CONFLICT(id) DO UPDATE SET body=excluded.body,expires=excluded.expires",
        params![e.id.as_slice(),e.claim_digest.0.as_slice(),expires,serde_json::to_string(e)?])?;
    Ok(())
}

pub(super) fn save_key(db: &Connection, c: &Credential) -> Result<()> {
    db.execute("INSERT INTO console_credentials VALUES(?1,?2) ON CONFLICT(id) DO UPDATE SET body=excluded.body",
        params![c.id.as_slice(),serde_json::to_string(c)?])?;
    Ok(())
}

pub(super) fn keys(db: &Connection) -> Result<DeviceKeys> {
    let mut statement = db.prepare("SELECT body FROM console_credentials ORDER BY rowid")?;
    let mut keys = Vec::new();
    for s in statement.query_map([], |r| r.get::<_, String>(0))? {
        keys.push(serde_json::from_str(&s?)?);
    }
    Ok(DeviceKeys(keys))
}
