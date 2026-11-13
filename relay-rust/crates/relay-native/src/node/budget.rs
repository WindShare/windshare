use super::settings::MonthlyBudget;
use crate::storage::{Database, budget::BudgetRecord};
use anyhow::{Result, ensure};
use chrono::{DateTime, Datelike, NaiveDate};

// Durable reservations precede socket permission, so a crash retains unused credit
// instead of granting another allowance. Batching avoids a SQLite commit per frame.
const RESERVATION_BYTES: u64 = 1024 * 1024;

pub(super) struct Budget {
    store: Database,
    record: Option<BudgetRecord>,
    available: u64,
}

impl Budget {
    pub fn new(store: Database) -> Result<Self> {
        let record = store.load_budget()?;
        Ok(Self {
            store,
            record,
            available: 0,
        })
    }

    pub fn update(&mut self, config: MonthlyBudget, now: i64) -> Result<()> {
        let period = period_start(now, config.reset_day_utc)?;
        let record = match self.record {
            Some(record) => {
                ensure!(
                    record.reset_day == config.reset_day_utc,
                    "billing reset day cannot change for an existing ledger"
                );
                if period <= record.period {
                    return Ok(());
                }
                BudgetRecord {
                    period,
                    reserved: 0,
                    ..record
                }
            }
            None => BudgetRecord {
                period,
                reset_day: config.reset_day_utc,
                reserved: 0,
            },
        };
        self.store.save_budget(record)?;
        self.record = Some(record);
        self.available = 0;
        tracing::info!(
            event = "relay_budget_period",
            period,
            reset_day = config.reset_day_utc
        );
        Ok(())
    }

    pub fn remaining(&self, limit: u64) -> u64 {
        self.record.map_or(limit, |record| {
            limit.saturating_sub(record.reserved.saturating_sub(self.available))
        })
    }

    pub fn period(&self) -> i64 {
        self.record.map_or(0, |record| record.period)
    }

    pub fn try_reserve(&mut self, limit: u64, requested: u64) -> Option<u64> {
        let remaining = self.remaining(limit);
        if self.available == 0 && remaining > 0 {
            return None;
        }
        let granted = requested.min(self.available).min(remaining);
        self.available -= granted;
        Some(granted)
    }

    pub fn reserve(&mut self, limit: u64, requested: u64) -> Result<u64> {
        let mut record = self.record.expect("configured budget initialized");
        self.available = self.available.min(self.remaining(limit));
        if self.available < requested && record.reserved < limit {
            let allocation = RESERVATION_BYTES
                .max(requested)
                .min(limit - record.reserved);
            record.reserved += allocation;
            // Durable upper bounds precede socket permission. Storage errors fail closed.
            self.store.save_budget(record)?;
            self.record = Some(record);
            self.available += allocation;
        }
        let granted = requested.min(self.available);
        self.available -= granted;
        Ok(granted)
    }
}

fn period_start(now: i64, day: u8) -> Result<i64> {
    let date = DateTime::from_timestamp(now, 0).ok_or_else(|| anyhow::anyhow!("invalid clock"))?;
    let (year, month) = if date.day() >= u32::from(day) {
        (date.year(), date.month())
    } else if date.month() == 1 {
        (date.year() - 1, 12)
    } else {
        (date.year(), date.month() - 1)
    };
    Ok(NaiveDate::from_ymd_opt(year, month, u32::from(day))
        .and_then(|date| date.and_hms_opt(0, 0, 0))
        .ok_or_else(|| anyhow::anyhow!("invalid billing period"))?
        .and_utc()
        .timestamp())
}

#[cfg(test)]
#[path = "budget_tests.rs"]
mod tests;
