use super::{Database, OptionalExtension};
use crate::node::contribution::Policy;
use anyhow::Result;

impl Database {
    pub(crate) fn contribution_policy(&self) -> Result<Policy> {
        let db = self.0.connection.lock().expect("node policy");
        let row: Option<String> = db
            .query_row(
                "SELECT policy FROM contribution WHERE singleton=1",
                [],
                |r| r.get(0),
            )
            .optional()?;
        drop(db);
        Ok(row
            .map(|v| serde_json::from_str(&v))
            .transpose()?
            .unwrap_or_default())
    }
}

pub(super) fn write(tx: &rusqlite::Transaction<'_>, policy: Policy) -> Result<bool> {
    let encoded = serde_json::to_string(&policy)?;
    let previous: Option<String> = tx
        .query_row(
            "SELECT policy FROM contribution WHERE singleton=1",
            [],
            |r| r.get(0),
        )
        .optional()?;
    let previous: Policy = previous
        .map(|v| serde_json::from_str(&v))
        .transpose()?
        .unwrap_or_default();
    tx.execute(
        "INSERT INTO contribution VALUES (1,?1) ON CONFLICT(singleton) DO UPDATE SET policy=excluded.policy",
        [encoded],
    )?;
    Ok(previous.enabled && !policy.enabled)
}
