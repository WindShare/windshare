use crate::storage::budget::{BudgetRecord, Window};
use anyhow::{Result, anyhow, ensure};
use chrono::{DateTime, Datelike, Months, NaiveDate};

// Persist a high-water mark before issuing credit. A crash can lose a batch
// of unused credit, but restarting cannot spend it twice.
const RESERVATION_BYTES: u64 = 1024 * 1024;

#[derive(Default)]
pub(super) struct Ledger {
    record: Option<BudgetRecord>,
    credit: u64,
}

impl Ledger {
    pub(super) const fn restore(record: Option<BudgetRecord>) -> Self {
        Self { record, credit: 0 }
    }

    pub(super) fn rollover(&self, window: Window, now: i64) -> Result<Option<BudgetRecord>> {
        let period = period_start(window, now)?;
        if let Some(record) = self.record {
            ensure!(
                record.window == window,
                "billing reset day cannot change for an existing ledger"
            );
            if period <= record.period {
                return Ok(None);
            }
        }
        Ok(Some(BudgetRecord {
            period,
            window,
            reserved: 0,
        }))
    }

    pub(super) fn advance(&mut self, next: Option<BudgetRecord>) {
        if let Some(record) = next {
            self.record = next;
            self.credit = 0;
            tracing::info!(event = "relay_budget_period", window = ?record.window, period = record.period);
        }
    }

    pub(super) fn day_elapsed(&self, timestamp: i64) -> bool {
        self.record.is_some_and(|record| {
            timestamp.saturating_sub(record.period) >= chrono::TimeDelta::days(1).num_seconds()
        })
    }

    pub(super) fn next_rollover(&self, window: Window) -> Option<i64> {
        let Some(record) = self.record else {
            return Some(i64::MIN);
        };
        if record.window != window {
            return Some(i64::MIN);
        }
        match window {
            Window::PublicDay => record
                .period
                .checked_add(chrono::TimeDelta::days(1).num_seconds()),
            Window::BillingMonth { .. } => DateTime::from_timestamp(record.period, 0)
                .and_then(|start| start.checked_add_months(Months::new(1)))
                .map(|next| next.timestamp()),
        }
    }

    pub(super) fn remaining(&self, limit: Option<u64>) -> u64 {
        limit.map_or(u64::MAX, |limit| {
            let reserved = self.record.map_or(0, |record| record.reserved);
            limit.saturating_sub(reserved - self.credit)
        })
    }

    pub(super) fn ready(&self, limit: Option<u64>) -> u64 {
        limit.map_or(u64::MAX, |_| self.credit.min(self.remaining(limit)))
    }

    pub(super) const fn spend(&mut self, limit: Option<u64>, bytes: u64) {
        if limit.is_some() {
            self.credit -= bytes;
        }
    }

    pub(super) fn refill(&self, limit: Option<u64>, requested: u64) -> Option<BudgetRecord> {
        let limit = limit?;
        let mut record = self.record.expect("configured ledger has a period");
        if self.credit >= requested || record.reserved >= limit {
            return None;
        }
        record.reserved += RESERVATION_BYTES
            .max(requested - self.credit)
            .min(limit - record.reserved);
        Some(record)
    }

    pub(super) const fn funded(&mut self, next: Option<BudgetRecord>) {
        if let Some(record) = next {
            self.credit += record.reserved - self.record.expect("active period").reserved;
            self.record = next;
        }
    }
}

pub(super) fn period_start(window: Window, now: i64) -> Result<i64> {
    let date = DateTime::from_timestamp(now, 0)
        .ok_or_else(|| anyhow!("invalid clock"))?
        .date_naive();
    let first = match window {
        Window::PublicDay => date,
        Window::BillingMonth { reset_day } => {
            let (year, month) = if date.day() >= u32::from(reset_day) {
                (date.year(), date.month())
            } else if date.month() == 1 {
                (date.year() - 1, 12)
            } else {
                (date.year(), date.month() - 1)
            };
            NaiveDate::from_ymd_opt(year, month, u32::from(reset_day))
                .ok_or_else(|| anyhow!("invalid billing period"))?
        }
    };
    Ok(first
        .and_hms_opt(0, 0, 0)
        .expect("midnight")
        .and_utc()
        .timestamp())
}
