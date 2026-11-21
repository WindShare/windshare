use crate::node::access::{AccountKey, Quota};
use anyhow::Result;
use rusqlite::{Connection, Statement, params};
use windshare_relay_access::{Issuer, entitlement::DAY_SECONDS, usage::AccountEntitlement};

// Fixed-width big-endian integers preserve protocol u64 ordering in SQLite.
pub(super) const SCHEMA: &str = "
    CREATE TABLE IF NOT EXISTS access_quota_day(
        singleton INTEGER PRIMARY KEY CHECK(singleton=1), day BLOB NOT NULL CHECK(length(day)=8)) STRICT;
    CREATE TABLE IF NOT EXISTS access_quotas(
        issuer BLOB NOT NULL, account BLOB NOT NULL,
        day BLOB NOT NULL CHECK(length(day)=8), version BLOB NOT NULL CHECK(length(version)=8),
        valid_until BLOB NOT NULL CHECK(length(valid_until)=8),
        remaining_bytes BLOB CHECK(length(remaining_bytes)=8),
        PRIMARY KEY(issuer,account)) STRICT, WITHOUT ROWID;
    CREATE INDEX IF NOT EXISTS access_quota_days ON access_quotas(day);
    CREATE INDEX IF NOT EXISTS access_quota_expiries ON access_quotas(valid_until)
        WHERE remaining_bytes IS NOT NULL;";

pub(super) fn day_floor(db: &Connection) -> Result<u64> {
    let day = db.query_row(
        "SELECT day FROM access_quota_day WHERE singleton=1",
        [],
        |row| row.get(0),
    )?;
    Ok(u64::from_be_bytes(day))
}

pub(super) fn prune(db: &Connection, now: u64) -> Result<u64> {
    db.execute(
        "INSERT INTO access_quota_day VALUES(1,?1)
         ON CONFLICT(singleton) DO UPDATE SET day=excluded.day WHERE excluded.day > day",
        [(now / DAY_SECONDS).to_be_bytes()],
    )?;
    let day = day_floor(db)?;
    let retired = db.execute(
        "DELETE FROM access_quotas WHERE day < ?1",
        [day.to_be_bytes()],
    )?;
    let expired = db.execute(
        "UPDATE access_quotas SET remaining_bytes=NULL
         WHERE remaining_bytes IS NOT NULL AND valid_until <= ?1",
        [now.to_be_bytes()],
    )?;
    if retired != 0 || expired != 0 {
        tracing::debug!(event = "quota_prune_staged", day, retired, expired);
    }
    Ok(day)
}

pub(super) fn restore(db: &Connection) -> Result<Vec<(AccountKey, Quota)>> {
    let mut statement = db.prepare(
        "SELECT issuer,account,day,version,valid_until,remaining_bytes FROM access_quotas",
    )?;
    let rows = statement.query_map([], |row| {
        let version = row.get::<_, [u8; 8]>(3)?;
        let valid_until = row.get::<_, [u8; 8]>(4)?;
        let remaining_bytes = row.get::<_, Option<[u8; 8]>>(5)?.map(u64::from_be_bytes);
        Ok((
            (row.get(0)?, row.get(1)?),
            Quota {
                day: u64::from_be_bytes(row.get(2)?),
                version: u64::from_be_bytes(version),
                valid_until: u64::from_be_bytes(valid_until),
                remaining_bytes,
            },
        ))
    })?;
    Ok(rows.collect::<rusqlite::Result<_>>()?)
}

pub(super) fn prepare_upsert(db: &Connection) -> Result<Statement<'_>> {
    Ok(db.prepare(
        "INSERT INTO access_quotas VALUES(?1,?2,?3,?4,?5,?6)
         ON CONFLICT(issuer,account) DO UPDATE SET day=excluded.day,version=excluded.version,
             valid_until=excluded.valid_until,remaining_bytes=excluded.remaining_bytes
         WHERE day < ?7
            OR (excluded.day, excluded.version, excluded.valid_until) > (day, version, valid_until)",
    )?)
}

pub(super) fn save(
    upsert: &mut Statement<'_>,
    key: AccountKey,
    quota: Quota,
    day_floor: u64,
) -> Result<bool> {
    if quota.day < day_floor {
        return Ok(false);
    }
    let (issuer, account) = key;
    let changed = upsert.execute(params![
        issuer,
        account,
        quota.day.to_be_bytes(),
        quota.version.to_be_bytes(),
        quota.valid_until.to_be_bytes(),
        quota.remaining_bytes.map(u64::to_be_bytes),
        day_floor.to_be_bytes(),
    ])?;
    Ok(changed != 0)
}

pub(super) fn save_batch(
    db: &Connection,
    issuer: Issuer,
    snapshots: Vec<AccountEntitlement>,
    day_floor: u64,
    now: u64,
) -> Result<Vec<(AccountKey, Quota)>> {
    let mut changes = Vec::with_capacity(snapshots.len());
    if snapshots.is_empty() {
        return Ok(changes);
    }
    let day = day_floor.max(now / DAY_SECONDS);
    let mut upsert = prepare_upsert(db)?;
    for snapshot in snapshots {
        let key = (issuer, snapshot.account);
        let quota = Quota::from_snapshot(snapshot.entitlement, now);
        if save(&mut upsert, key, quota, day)? {
            changes.push((key, quota));
        }
    }
    Ok(changes)
}
