use super::Database;
use anyhow::{Result, anyhow};
use rusqlite::{OptionalExtension, params};

#[derive(Clone, Copy, Debug, Default)]
pub struct BudgetRecord {
    pub period: i64,
    pub reset_day: u8,
    pub reserved: u64,
}

impl Database {
    pub(crate) fn load_budget(&self) -> Result<Option<BudgetRecord>> {
        let connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database poisoned"))?;
        connection.execute_batch(
            "CREATE TABLE IF NOT EXISTS relay_budget (
                singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
                period INTEGER NOT NULL,
                reset_day INTEGER NOT NULL CHECK(reset_day BETWEEN 1 AND 28),
                reserved INTEGER NOT NULL CHECK(reserved >= 0)
             ) STRICT;",
        )?;
        Ok(connection
            .query_row(
                "SELECT period, reset_day, reserved FROM relay_budget WHERE singleton=1",
                [],
                |row| {
                    Ok(BudgetRecord {
                        period: row.get(0)?,
                        reset_day: row.get(1)?,
                        reserved: u64::try_from(row.get::<_, i64>(2)?).map_err(|error| {
                            rusqlite::Error::FromSqlConversionFailure(
                                2,
                                rusqlite::types::Type::Integer,
                                Box::new(error),
                            )
                        })?,
                    })
                },
            )
            .optional()?)
    }

    pub(crate) fn save_budget(&self, record: BudgetRecord) -> Result<()> {
        let reserved = i64::try_from(record.reserved)?;
        let connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database poisoned"))?;
        connection.execute(
            "INSERT INTO relay_budget VALUES (1, ?1, ?2, ?3)
             ON CONFLICT(singleton) DO UPDATE SET period=excluded.period,
                 reset_day=excluded.reset_day, reserved=excluded.reserved",
            params![record.period, record.reset_day, reserved],
        )?;
        drop(connection);
        Ok(())
    }
}
