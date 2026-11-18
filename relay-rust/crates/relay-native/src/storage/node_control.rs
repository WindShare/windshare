use super::Database;
use crate::node::access::Cache;
use anyhow::{Result, anyhow};
use rusqlite::{OptionalExtension, params};
use windshare_relay_access::usage::Batch;

impl Database {
    pub(crate) fn initialize_control(&self) -> Result<Cache> {
        let connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database lock"))?;
        connection.execute_batch("CREATE TABLE IF NOT EXISTS usage_outbox(
            id BLOB PRIMARY KEY, issuer BLOB NOT NULL, body BLOB NOT NULL, created_at INTEGER NOT NULL) STRICT;
            CREATE TABLE IF NOT EXISTS access_cache(singleton INTEGER PRIMARY KEY CHECK(singleton=1), body BLOB NOT NULL) STRICT;")?;
        let bytes: Option<Vec<u8>> = connection
            .query_row(
                "SELECT body FROM access_cache WHERE singleton=1",
                [],
                |row| row.get(0),
            )
            .optional()?;
        drop(connection);
        bytes.map_or_else(
            || Ok(Cache::default()),
            |bytes| Ok(serde_json::from_slice(&bytes)?),
        )
    }

    pub(crate) fn save_access(&self, next: Cache) -> Result<Cache> {
        let mut connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database lock"))?;
        let transaction = connection.transaction()?;
        let bytes: Option<Vec<u8>> = transaction
            .query_row(
                "SELECT body FROM access_cache WHERE singleton=1",
                [],
                |row| row.get(0),
            )
            .optional()?;
        let mut cache: Cache = bytes.map_or_else(
            || Ok(Cache::default()),
            |bytes| serde_json::from_slice(&bytes),
        )?;
        cache.merge(next)?;
        transaction.execute("INSERT INTO access_cache VALUES(1,?1) ON CONFLICT(singleton) DO UPDATE SET body=excluded.body",
            [serde_json::to_vec(&cache)?])?;
        transaction.commit()?;
        drop(connection);
        Ok(cache)
    }

    pub(crate) fn enqueue_usage(&self, batches: &[Batch]) -> Result<()> {
        let mut connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database lock"))?;
        let transaction = connection.transaction()?;
        for batch in batches {
            transaction.execute(
                "INSERT INTO usage_outbox VALUES(?1,?2,?3,?4)",
                params![
                    batch.id.as_slice(),
                    batch.issuer.as_slice(),
                    serde_json::to_vec(batch)?,
                    i64::try_from(batch.created_at)?
                ],
            )?;
        }
        transaction.commit()?;
        drop(connection);
        Ok(())
    }

    pub(crate) fn pending_usage(&self, issuer: [u8; 32]) -> Result<Option<Batch>> {
        let connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database lock"))?;
        let bytes: Option<Vec<u8>> = connection
            .query_row(
                "SELECT body FROM usage_outbox WHERE issuer=?1 ORDER BY created_at,id LIMIT 1",
                [issuer.as_slice()],
                |row| row.get(0),
            )
            .optional()?;
        drop(connection);
        bytes
            .map(|bytes| Ok(serde_json::from_slice(&bytes)?))
            .transpose()
    }

    pub(crate) fn acknowledge_usage(&self, id: [u8; 16]) -> Result<()> {
        self.0
            .connection
            .lock()
            .map_err(|_| anyhow!("database lock"))?
            .execute("DELETE FROM usage_outbox WHERE id=?1", [id.as_slice()])?;
        Ok(())
    }
}
