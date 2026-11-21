mod quotas;

use super::Database;
use crate::node::access::{AccessSnapshot, AccessUpdate, AuthorityState};
use anyhow::{Result, anyhow};
use rusqlite::{Connection, OptionalExtension, params};
use windshare_relay_access::{
    Issuer, restrictions::Restrictions, trust::Generation, usage::AccountEntitlement,
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

const ADMISSION_SCHEMA: u32 = 1;

pub(super) fn restore(db: &Connection, now: u64) -> Result<AccessSnapshot> {
    db.execute_batch(SCHEMA)?;
    // initialize_control holds one transaction through migration and decoding.
    // Rewrite only the policy fields so each issuer keeps its highest trusted generation.
    let schema = db.pragma_query_value(None, "user_version", |row| row.get::<_, u32>(0))?;
    anyhow::ensure!(
        schema <= ADMISSION_SCHEMA,
        "unsupported node control schema"
    );
    if schema < ADMISSION_SCHEMA {
        db.execute(
            "UPDATE access_authorities SET body = CAST(json_set(
           json_remove(CAST(body AS TEXT), '$.policy.enabled'),
           '$.policy.admission', CASE json_extract(CAST(body AS TEXT), '$.policy.enabled')
                                  WHEN 1 THEN 'accepting' ELSE 'disabled' END,
           '$.policy.probe_account', json(?1)) AS BLOB)
         WHERE json_type(CAST(body AS TEXT), '$.policy.enabled') IN ('true','false')",
            [serde_json::to_string(
                &windshare_relay_access::probe::PROBE_ACCOUNT,
            )?],
        )?;
        db.pragma_update(None, "user_version", ADMISSION_SCHEMA)?;
    }
    db.execute_batch(quotas::SCHEMA)?;
    // Establish the durable UTC day floor even on nodes that start without a reporter.
    let quota_day_floor = quotas::prune(db, now)?;
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

#[cfg(test)]
#[path = "access_control_tests.rs"]
mod tests;
