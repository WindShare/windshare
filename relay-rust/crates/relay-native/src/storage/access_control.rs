mod quotas;

use super::Database;
use crate::node::access::{AccessSnapshot, AccessUpdate, AuthorityState, Quota};
use anyhow::{Result, anyhow, ensure};
use rusqlite::{Connection, OptionalExtension, params};
use serde::Deserialize;
use std::collections::BTreeMap;
use windshare_relay_access::{
    AccountId, Issuer,
    entitlement::Entitlement,
    restrictions::Restrictions,
    trust::Generation,
    usage::{AccountEntitlement, NodePolicy},
};

const SCHEMA: &str = "
    CREATE TABLE IF NOT EXISTS access_authorities(
        issuer BLOB PRIMARY KEY, body BLOB NOT NULL) STRICT, WITHOUT ROWID;
    CREATE TABLE IF NOT EXISTS access_restrictions(
        issuer BLOB PRIMARY KEY, version BLOB NOT NULL, body BLOB NOT NULL) STRICT;";

impl Database {
    pub(crate) fn save_authority(&self, generation: Generation) -> Result<AuthorityState> {
        let connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database lock"))?;
        save_authority(&connection, generation.into())
    }

    pub(crate) fn save_status(
        &self,
        authority: AuthorityState,
        snapshots: Vec<AccountEntitlement>,
        now: u64,
    ) -> Result<AccessUpdate> {
        let mut connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database lock"))?;
        let transaction = connection.transaction()?;
        let authority = save_authority(&transaction, authority)?;
        let quota_day_floor = quotas::day_floor(&transaction)?;
        let quotas = quotas::save_batch(
            &transaction,
            authority.generation.issuer,
            snapshots,
            quota_day_floor,
            now,
        )?;
        transaction.commit()?;
        drop(connection);
        Ok(AccessUpdate {
            authority,
            quotas,
            quota_day_floor,
        })
    }

    pub(crate) fn prune_quotas(&self, now: u64) -> Result<u64> {
        let mut connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database lock"))?;
        let transaction = connection.transaction()?;
        let day = quotas::prune(&transaction, now)?;
        transaction.commit()?;
        drop(connection);
        Ok(day)
    }

    pub(crate) fn save_restrictions(
        &self,
        issuer: Issuer,
        restrictions: &Restrictions,
    ) -> Result<()> {
        let connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database lock"))?;
        write_restrictions(&connection, issuer, restrictions)
    }
}

pub(super) fn restore(db: &Connection, now: u64) -> Result<AccessSnapshot> {
    db.execute_batch(SCHEMA)?;
    db.execute_batch(quotas::SCHEMA)?;
    // Establish the durable UTC day floor even on nodes that start without a reporter.
    let quota_day_floor = quotas::prune(db, now)?;
    migrate_cache(db, quota_day_floor, now)?;
    let mut statement = db.prepare("SELECT body FROM access_authorities")?;
    let authorities = statement
        .query_map([], |row| row.get::<_, Vec<u8>>(0))?
        .map(|row| Ok(serde_json::from_slice(&row?)?))
        .collect::<Result<_>>()?;
    let mut statement = db.prepare("SELECT issuer,body FROM access_restrictions")?;
    let restrictions = statement
        .query_map([], |row| {
            Ok((row.get::<_, Issuer>(0)?, row.get::<_, Vec<u8>>(1)?))
        })?
        .map(|row| {
            let (issuer, body) = row?;
            Ok((issuer, serde_json::from_slice(&body)?))
        })
        .collect::<Result<_>>()?;
    Ok(AccessSnapshot {
        authorities,
        restrictions,
        quotas: quotas::restore(db)?,
        quota_day_floor,
    })
}

pub(super) fn save_authority(db: &Connection, next: AuthorityState) -> Result<AuthorityState> {
    let issuer = next.generation.issuer;
    let body: Option<Vec<u8>> = db
        .query_row(
            "SELECT body FROM access_authorities WHERE issuer=?1",
            [issuer],
            |row| row.get(0),
        )
        .optional()?;
    let mut current = next;
    if let Some(body) = body {
        let previous: AuthorityState = serde_json::from_slice(&body)?;
        current = previous;
        current.merge(next)?;
        if current == previous {
            return Ok(current);
        }
    }
    db.execute(
        "INSERT INTO access_authorities VALUES(?1,?2)
         ON CONFLICT(issuer) DO UPDATE SET body=excluded.body",
        params![issuer, serde_json::to_vec(&current)?],
    )?;
    Ok(current)
}

fn write_restrictions(db: &Connection, issuer: Issuer, restrictions: &Restrictions) -> Result<()> {
    // Big-endian bytes preserve the unsigned protocol version's order in SQLite.
    db.execute(
        "INSERT INTO access_restrictions VALUES(?1,?2,?3)
         ON CONFLICT(issuer) DO UPDATE SET version=excluded.version,body=excluded.body
         WHERE excluded.version > access_restrictions.version",
        params![
            issuer,
            restrictions.version.to_be_bytes(),
            serde_json::to_vec(restrictions)?
        ],
    )?;
    Ok(())
}

#[derive(Deserialize)]
struct LegacyCache {
    generations: Vec<Generation>,
    entitlements: Vec<(Issuer, AccountId, Entitlement)>,
    restrictions: Vec<(Issuer, Restrictions)>,
    policies: Vec<(Issuer, NodePolicy)>,
}

fn migrate_cache(db: &Connection, quota_day_floor: u64, now: u64) -> Result<()> {
    let exists: bool = db.query_row(
        "SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='access_cache')",
        [],
        |row| row.get(0),
    )?;
    if !exists {
        return Ok(());
    }
    let body: Option<Vec<u8>> = db
        .query_row(
            "SELECT body FROM access_cache WHERE singleton=1",
            [],
            |row| row.get(0),
        )
        .optional()?;
    if let Some(body) = body {
        let legacy: LegacyCache = serde_json::from_slice(&body)?;
        let mut policies: BTreeMap<_, _> = legacy.policies.into_iter().collect();
        for generation in legacy.generations {
            let policy = policies.remove(&generation.issuer);
            save_authority(db, AuthorityState { generation, policy })?;
        }
        ensure!(policies.is_empty(), "missing cached issuer generation");
        let mut upsert = quotas::prepare_upsert(db)?;
        for (issuer, account, entitlement) in legacy.entitlements {
            quotas::save(
                &mut upsert,
                (issuer, account),
                Quota::from_snapshot(entitlement, now),
                quota_day_floor,
            )?;
        }
        for (issuer, restrictions) in legacy.restrictions {
            write_restrictions(db, issuer, &restrictions)?;
        }
    }
    // The old row contains the only rollback barriers on upgrade, so retire it in
    // the same transaction that installs the new records.
    db.execute("DROP TABLE access_cache", [])?;
    Ok(())
}

#[cfg(test)]
#[path = "access_control_tests.rs"]
mod tests;
