use super::Database;
use crate::node::access::AccessSnapshot;
use anyhow::{Result, anyhow, ensure};
use rusqlite::{OptionalExtension, params};
use windshare_relay_access::{Issuer, usage::Batch};

impl Database {
    pub(crate) fn initialize_control(&self, now: u64) -> Result<AccessSnapshot> {
        let mut connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database lock"))?;
        let transaction = connection.transaction()?;
        transaction.execute_batch("CREATE TABLE IF NOT EXISTS usage_outbox(
            id BLOB PRIMARY KEY, issuer BLOB NOT NULL, body BLOB NOT NULL, created_at INTEGER NOT NULL) STRICT;
            CREATE TABLE IF NOT EXISTS usage_outbox_quarantine(
             id BLOB PRIMARY KEY, issuer BLOB NOT NULL, body BLOB NOT NULL, created_at INTEGER NOT NULL,
             reason TEXT NOT NULL) STRICT;")?;
        let snapshot = super::access_control::restore(&transaction, now)?;
        transaction.commit()?;
        drop(connection);
        Ok(snapshot)
    }

    pub(crate) fn enqueue_usage(&self, batches: &[Batch]) -> Result<()> {
        for batch in batches {
            ensure!(
                valid_usage_batch(batch),
                "invalid usage batch {:?}",
                batch.id
            );
        }
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

    /// A damaged queue head must not prevent later usage from being reported.
    pub(crate) fn next_usage_batch(&self, issuer: Issuer) -> Result<Option<Batch>> {
        loop {
            let entry: Option<(Vec<u8>, Vec<u8>)> = self
                .0
                .connection
                .lock()
                .map_err(|_| anyhow!("database lock"))?
                .query_row(
                    "SELECT id,body FROM usage_outbox WHERE issuer=?1 ORDER BY created_at,id LIMIT 1",
                    [issuer.as_slice()],
                    |row| Ok((row.get(0)?, row.get(1)?)),
                )
                .optional()?;
            let Some((id, body)) = entry else {
                return Ok(None);
            };
            // Local, time-independent validation can prove rejection before
            // contacting the authority, whose errors may be transient.
            let reason = match serde_json::from_slice::<Batch>(&body) {
                Ok(batch) if batch.id.as_slice() != id || batch.issuer != issuer => {
                    "metadata_mismatch"
                }
                Ok(batch) if valid_usage_batch(&batch) => return Ok(Some(batch)),
                Ok(_) => "invalid_usage_batch",
                Err(_) => "invalid_json",
            };
            self.quarantine_usage(issuer, &id, &body, reason)?;
        }
    }

    fn quarantine_usage(&self, issuer: Issuer, id: &[u8], body: &[u8], reason: &str) -> Result<()> {
        let mut connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database lock"))?;
        let transaction = connection.transaction()?;
        // Keep the original bytes for accounting recovery, even if they cannot be decoded.
        let moved = transaction.execute(
            "INSERT INTO usage_outbox_quarantine (id,issuer,body,created_at,reason)
             SELECT id,issuer,body,created_at,?4 FROM usage_outbox
             WHERE id=?1 AND issuer=?2 AND body=?3",
            params![id, issuer.as_slice(), body, reason],
        )?;
        if moved == 0 {
            return Ok(());
        }
        transaction.execute("DELETE FROM usage_outbox WHERE id=?1", [id])?;
        transaction.commit()?;
        drop(connection);
        tracing::error!(event = "usage_batch_quarantined", batch = ?id, ?issuer, reason);
        Ok(())
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

fn valid_usage_batch(batch: &Batch) -> bool {
    !batch.records.is_empty() && batch.valid()
}
