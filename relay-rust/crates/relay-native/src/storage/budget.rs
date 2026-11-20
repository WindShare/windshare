use super::Database;
use anyhow::{Result, anyhow};
use rusqlite::{OptionalExtension, params};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Window {
    BillingMonth { reset_day: u8 },
    PublicDay,
}

#[derive(Clone, Copy, Debug)]
pub struct BudgetRecord {
    pub period: i64,
    pub window: Window,
    pub reserved: u64,
}

#[derive(Clone, Copy, Default)]
pub struct BudgetRecords {
    pub monthly: Option<BudgetRecord>,
    pub public_daily: Option<BudgetRecord>,
}

impl BudgetRecords {
    pub const fn is_empty(self) -> bool {
        self.monthly.is_none() && self.public_daily.is_none()
    }
}

impl Database {
    pub(crate) fn persist_budgets(&self, records: BudgetRecords) -> Result<()> {
        self.budget_transaction(records, |_| Ok(()))
    }

    pub(super) fn budget_transaction<T>(
        &self,
        records: BudgetRecords,
        prepare: impl FnOnce(&rusqlite::Transaction<'_>) -> Result<T>,
    ) -> Result<T> {
        let mut connection = self
            .0
            .connection
            .lock()
            .map_err(|_| anyhow!("database poisoned"))?;
        let transaction = connection.transaction()?;
        write_budgets(&transaction, records)?;
        let prepared = prepare(&transaction)?;
        transaction.commit()?;
        drop(connection);
        Ok(prepared)
    }

    pub(crate) fn load_budgets(&self) -> Result<BudgetRecords> {
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
             ) STRICT;
             CREATE TABLE IF NOT EXISTS public_relay_day (
                singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
                period INTEGER NOT NULL,
                reserved INTEGER NOT NULL CHECK(reserved >= 0)
             ) STRICT;",
        )?;
        let monthly = connection
            .query_row(
                "SELECT period, reset_day, reserved FROM relay_budget WHERE singleton=1",
                [],
                |row| {
                    Ok(BudgetRecord {
                        period: row.get(0)?,
                        window: Window::BillingMonth {
                            reset_day: row.get(1)?,
                        },
                        reserved: reserved_bytes(row, 2)?,
                    })
                },
            )
            .optional()?;
        let public_daily = connection
            .query_row(
                "SELECT period, reserved FROM public_relay_day WHERE singleton=1",
                [],
                |row| {
                    Ok(BudgetRecord {
                        period: row.get(0)?,
                        window: Window::PublicDay,
                        reserved: reserved_bytes(row, 1)?,
                    })
                },
            )
            .optional()?;
        drop(connection);
        Ok(BudgetRecords {
            monthly,
            public_daily,
        })
    }
}

fn reserved_bytes(row: &rusqlite::Row<'_>, column: usize) -> rusqlite::Result<u64> {
    let value: i64 = row.get(column)?;
    u64::try_from(value).map_err(|_| rusqlite::Error::IntegralValueOutOfRange(column, value))
}

fn write_budgets(connection: &rusqlite::Connection, records: BudgetRecords) -> Result<()> {
    for record in [records.monthly, records.public_daily]
        .into_iter()
        .flatten()
    {
        let reserved = i64::try_from(record.reserved)?;
        match record.window {
            Window::BillingMonth { reset_day } => connection.execute(
                "INSERT INTO relay_budget VALUES (1, ?1, ?2, ?3)
                 ON CONFLICT(singleton) DO UPDATE SET period=excluded.period,
                     reset_day=excluded.reset_day, reserved=excluded.reserved",
                params![record.period, reset_day, reserved],
            )?,
            Window::PublicDay => connection.execute(
                "INSERT INTO public_relay_day VALUES (1, ?1, ?2)
                 ON CONFLICT(singleton) DO UPDATE SET period=excluded.period, reserved=excluded.reserved",
                params![record.period, reserved],
            )?,
        };
    }
    Ok(())
}
