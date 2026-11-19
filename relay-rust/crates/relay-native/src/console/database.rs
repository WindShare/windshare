use super::{Rejected, Result};
use crate::node::access::{BoundAccount, DeviceCredential, DeviceKeys};
use anyhow::ensure;
use hyper::StatusCode;
use rusqlite::{Connection, OptionalExtension, Transaction, params};
use windshare_relay_access::{Credential, Enrollment, EnrollmentState};

const ACTIVE_CREDENTIALS: &str = "json_extract(body, '$.state') = 'Active'";

pub(super) struct Management<'db> {
    pub db: Transaction<'db>,
    credential: Option<DeviceCredential>,
}

impl<'db> Management<'db> {
    pub(super) fn begin(db: &'db mut Connection) -> Result<Self> {
        Ok(Self {
            db: db.transaction()?,
            credential: None,
        })
    }

    pub(super) fn save_credential(&mut self, key: Credential) -> Result<bool> {
        ensure!(
            self.credential.is_none(),
            "one credential change per management transaction"
        );
        let changed = save_key(&self.db, &key)?;
        if changed {
            let account = binding(&self.db, &key.id)?;
            self.credential = Some(DeviceCredential { key, account });
        }
        Ok(changed)
    }

    pub(super) fn commit(self) -> Result<Option<DeviceCredential>> {
        self.db.commit()?;
        Ok(self.credential)
    }
}

pub(super) fn initialize(db: &Connection) -> Result<()> {
    db.execute_batch("CREATE TABLE IF NOT EXISTS console_owner (id INTEGER PRIMARY KEY CHECK(id=1), password TEXT NOT NULL) STRICT;
        CREATE TABLE IF NOT EXISTS console_sessions (digest BLOB PRIMARY KEY, expires INTEGER NOT NULL) STRICT;
        CREATE TABLE IF NOT EXISTS console_enrollments (id BLOB PRIMARY KEY, claimant BLOB UNIQUE NOT NULL, expires INTEGER, body TEXT NOT NULL) STRICT;
        CREATE TABLE IF NOT EXISTS console_account_bindings (credential BLOB PRIMARY KEY, issuer BLOB NOT NULL, account BLOB NOT NULL) STRICT;
        CREATE TABLE IF NOT EXISTS console_credentials (id BLOB PRIMARY KEY, body TEXT NOT NULL) STRICT;
        CREATE TABLE IF NOT EXISTS console_attempts (scope TEXT PRIMARY KEY, expires INTEGER NOT NULL, attempts INTEGER NOT NULL) STRICT;")?;
    db.execute_batch(&format!(
        "CREATE INDEX IF NOT EXISTS console_active_credentials
         ON console_credentials(id) WHERE {ACTIVE_CREDENTIALS};"
    ))?;
    Ok(())
}

pub(super) fn active_credentials(db: &Connection) -> Result<usize> {
    let count: u32 = db.query_row(
        &format!("SELECT count(*) FROM console_credentials WHERE {ACTIVE_CREDENTIALS}"),
        [],
        |row| row.get(0),
    )?;
    Ok(usize::try_from(count)?)
}

pub(super) fn cleanup(db: &Connection, now: u64) -> Result<()> {
    let now = i64::try_from(now)?;
    db.execute("DELETE FROM console_account_bindings WHERE credential IN (SELECT id FROM console_enrollments WHERE expires<=?1) AND credential NOT IN (SELECT id FROM console_credentials)", [now])?;
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

pub(super) fn save_key(db: &Connection, c: &Credential) -> Result<bool> {
    let changed = db.execute(
        "INSERT INTO console_credentials VALUES(?1,?2)
         ON CONFLICT(id) DO UPDATE SET body=excluded.body WHERE body<>excluded.body",
        params![c.id.as_slice(), serde_json::to_string(c)?],
    )?;
    Ok(changed != 0)
}

pub(super) fn binding(db: &Connection, credential: &[u8; 16]) -> Result<Option<BoundAccount>> {
    Ok(db
        .query_row(
            "SELECT issuer, account FROM console_account_bindings WHERE credential=?1",
            [credential.as_slice()],
            |row| {
                Ok(BoundAccount {
                    issuer: row.get(0)?,
                    account: row.get(1)?,
                })
            },
        )
        .optional()?)
}
pub(super) fn keys(db: &Connection) -> Result<DeviceKeys> {
    let mut statement = db.prepare(
        "SELECT body, issuer, account FROM console_credentials
         LEFT JOIN console_account_bindings ON id=credential
         ORDER BY console_credentials.rowid",
    )?;
    let rows = statement.query_map([], |row| {
        let issuer: Option<_> = row.get(1)?;
        let account = issuer
            .map(|issuer| -> rusqlite::Result<_> {
                Ok(BoundAccount {
                    issuer,
                    account: row.get(2)?,
                })
            })
            .transpose()?;
        Ok((row.get::<_, String>(0)?, account))
    })?;
    let keys = rows
        .map(|row| {
            let (body, account) = row?;
            Ok(DeviceCredential {
                key: serde_json::from_str(&body)?,
                account,
            })
        })
        .collect::<Result<_>>()?;
    Ok(DeviceKeys(keys))
}
