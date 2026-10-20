use super::{Database, failure};
use crate::turn::Lease;
use std::collections::BTreeMap;
use worker::{Result, SqlStorageValue};

const LEASE_PREFIX: &str = "turn_lease:";
const RETENTION_MS: u64 = 60 * 60 * 1000;
const LEASE_ROWS: &str = "key GLOB 'turn_lease:*'";
const LIVE_ROWS: &str = "key GLOB 'turn_lease:*' AND json_extract(value,'$.state.state') IN ('issuing','withdrawing','active','revocation_pending')";
const REVOCATION_ROWS: &str =
    "key GLOB 'turn_lease:*' AND json_extract(value,'$.state.state')='revocation_pending'";

pub struct LeaseCapacity {
    pub deployment: u64,
    pub device: u64,
}

fn timestamp(value: u64) -> Result<SqlStorageValue> {
    Ok(i64::try_from(value).map_err(failure)?.into())
}
fn lease_key(id: &str) -> String {
    format!("{LEASE_PREFIX}{id}")
}

impl Database {
    pub(super) fn initialize_turn_queries(&self) -> Result<()> {
        // Terminal history must not increase the cost of admission and pending work.
        self.0.exec(
            &format!("CREATE INDEX IF NOT EXISTS turn_live_leases ON access_entries(json_extract(value,'$.expires_at'),json_extract(value,'$.subject.device')) WHERE {LIVE_ROWS}"),
            None,
        )?;
        self.0.exec(
            &format!("CREATE INDEX IF NOT EXISTS turn_revocations ON access_entries(json_extract(value,'$.state.next_attempt'),key) WHERE {REVOCATION_ROWS}"),
            None,
        )?;
        Ok(())
    }

    pub fn turn_lease(&self, id: &str) -> Result<Option<Lease>> {
        self.get(&lease_key(id))
    }

    pub fn insert_turn_lease(&self, lease: &Lease) -> Result<()> {
        // A reservation cannot overwrite the result of an earlier request with this ID.
        self.0.exec(
            "INSERT INTO access_entries(key,value,expires) VALUES(?,?,?)",
            Some(vec![
                lease_key(&lease.id).into(),
                serde_json::to_string(lease)?.into(),
                timestamp(lease.expires_at.saturating_add(RETENTION_MS))?,
            ]),
        )?;
        Ok(())
    }

    pub fn update_turn_lease<T>(
        &self,
        id: &str,
        transition: impl FnOnce(&mut Lease) -> Result<T>,
    ) -> Result<Option<(Lease, T)>> {
        // This synchronous boundary cannot yield between reading current authority
        // and saving its transition. External callbacks carry identities, not snapshots.
        let Some(mut lease) = self.turn_lease(id)? else {
            return Ok(None);
        };
        let previous = lease.clone();
        let result = transition(&mut lease)?;
        if lease != previous {
            self.put(
                &lease_key(id),
                &lease,
                Some(lease.expires_at.saturating_add(RETENTION_MS)),
            )?;
        }
        Ok(Some((lease, result)))
    }

    pub fn turn_capacity(&self, device: &str, now: u64) -> Result<LeaseCapacity> {
        // Reservations end at their issuance deadline; a pending provider revocation
        // still owns capacity until confirmation or the conservative provider expiry.
        let rows = self.0.exec(
            &format!("SELECT COUNT(*),COALESCE(SUM(json_extract(value,'$.subject.device')=?),0) FROM access_entries WHERE {LIVE_ROWS} AND json_extract(value,'$.expires_at')>? AND COALESCE(json_extract(value,'$.state.deadline'),json_extract(value,'$.expires_at'))>?"),
            Some(vec![device.into(), timestamp(now)?, timestamp(now)?]),
        )?;
        match rows.raw().next().transpose()?.as_deref() {
            Some(
                [
                    SqlStorageValue::Integer(deployment),
                    SqlStorageValue::Integer(device),
                ],
            ) => Ok(LeaseCapacity {
                deployment: u64::try_from(*deployment).map_err(failure)?,
                device: u64::try_from(*device).map_err(failure)?,
            }),
            _ => Err(failure("invalid TURN capacity counts")),
        }
    }

    pub fn open_turn_leases(&self, device: Option<&str>) -> Result<Vec<String>> {
        let rows = self.0.exec(
            &format!("SELECT json_extract(value,'$.id') FROM access_entries WHERE {LIVE_ROWS} AND (? IS NULL OR json_extract(value,'$.subject.device')=?) ORDER BY key"),
            Some(vec![
                device.map_or(SqlStorageValue::Null, Into::into),
                device.map_or(SqlStorageValue::Null, Into::into),
            ]),
        )?;
        rows.raw()
            .map(|row| match row?.as_slice() {
                [SqlStorageValue::String(id)] => Ok(id.clone()),
                _ => Err(failure("invalid TURN lease identity")),
            })
            .collect()
    }

    pub fn due_turn_revocations(&self, now: u64, limit: usize) -> Result<Vec<String>> {
        let rows = self.0.exec(
            &format!("SELECT json_extract(value,'$.id') FROM access_entries WHERE {REVOCATION_ROWS} AND json_extract(value,'$.state.next_attempt')<=? AND json_extract(value,'$.expires_at')>? ORDER BY json_extract(value,'$.state.next_attempt'),key LIMIT ?"),
            Some(vec![timestamp(now)?, timestamp(now)?, i64::try_from(limit).map_err(failure)?.into()]),
        )?;
        rows.raw()
            .map(|row| match row?.as_slice() {
                [SqlStorageValue::String(id)] => Ok(id.clone()),
                _ => Err(failure("invalid TURN revocation identity")),
            })
            .collect()
    }

    pub fn next_turn_deadline(&self) -> Result<Option<u64>> {
        let rows = self.0.exec(
            &format!("SELECT MIN(MIN(json_extract(value,'$.expires_at'),COALESCE(json_extract(value,'$.state.deadline'),json_extract(value,'$.state.next_attempt'),json_extract(value,'$.expires_at')))) FROM access_entries WHERE {LIVE_ROWS}"),
            None,
        )?;
        match rows.raw().next().transpose()?.as_deref() {
            Some([SqlStorageValue::Null]) | None => Ok(None),
            Some([SqlStorageValue::Integer(at)]) => u64::try_from(*at).map(Some).map_err(failure),
            _ => Err(failure("invalid TURN work deadline")),
        }
    }

    pub fn turn_lease_counts(&self, now: u64) -> Result<BTreeMap<String, u64>> {
        // Project time-dependent status without loading grants or truncating history.
        let rows = self.0.exec(
            &format!("SELECT CASE
                WHEN json_extract(value,'$.state.state')='revoked' THEN 'revoked'
                WHEN json_extract(value,'$.expires_at')<=? THEN 'expired'
                WHEN json_extract(value,'$.state.state') IN ('issuing','withdrawing') AND json_extract(value,'$.state.deadline')<=? THEN 'issuance_failed'
                WHEN json_extract(value,'$.state.state')='withdrawing' THEN 'revocation_pending'
                ELSE json_extract(value,'$.state.state') END AS status,COUNT(*)
                FROM access_entries WHERE {LEASE_ROWS} GROUP BY status"),
            Some(vec![timestamp(now)?, timestamp(now)?]),
        )?;
        rows.raw()
            .map(|row| match row?.as_slice() {
                [
                    SqlStorageValue::String(status),
                    SqlStorageValue::Integer(count),
                ] => Ok((status.clone(), u64::try_from(*count).map_err(failure)?)),
                _ => Err(failure("invalid TURN status counts")),
            })
            .collect()
    }
}
