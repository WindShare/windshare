use super::{Database, OptionalExtension};
use anyhow::Result;

impl Database {
    pub(crate) fn cached_peers(&self, maximum: usize) -> Result<Option<Vec<u8>>> {
        let db = self.0.connection.lock().expect("discovery cache");
        Ok(db
            .query_row(
                "SELECT records FROM discovery_cache WHERE singleton=1 AND length(records)<=?1",
                [i64::try_from(maximum)?],
                |row| row.get(0),
            )
            .optional()?)
    }

    pub(crate) fn cache_peers(&self, records: &[u8]) -> Result<()> {
        self.0.connection.lock().expect("discovery cache").execute(
            "INSERT INTO discovery_cache VALUES (1,?1) ON CONFLICT(singleton) DO UPDATE SET records=excluded.records",[records])?;
        Ok(())
    }
}
