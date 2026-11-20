mod egress;
mod ledger;

use super::Settings;
use crate::storage::budget::{BudgetRecords, Window};
use anyhow::Result;
pub(super) use egress::WriteAttempt;
pub use egress::WriteOrigin;
use ledger::Ledger;

#[derive(Clone, Copy)]
struct Limits {
    monthly: Option<u64>,
    public_daily: Option<u64>,
}

#[derive(Default)]
pub(super) struct Budgets {
    monthly: Ledger,
    public_daily: Ledger,
}

impl Budgets {
    pub(super) const fn restore(records: BudgetRecords) -> Self {
        Self {
            monthly: Ledger::restore(records.monthly),
            public_daily: Ledger::restore(records.public_daily),
        }
    }

    pub(super) fn rollover(&self, settings: &Settings, now: i64) -> Result<BudgetRecords> {
        Ok(BudgetRecords {
            monthly: match settings.relay_budget {
                Some(config) => self.monthly.rollover(
                    Window::BillingMonth {
                        reset_day: config.reset_day_utc,
                    },
                    now,
                )?,
                None => None,
            },
            public_daily: match settings.contribution.daily_bytes {
                Some(_) => self.public_daily.rollover(Window::PublicDay, now)?,
                None => None,
            },
        })
    }

    pub(super) fn advance(&mut self, records: BudgetRecords) {
        self.monthly.advance(records.monthly);
        self.public_daily.advance(records.public_daily);
    }

    pub(super) fn node_remaining(&self, settings: &Settings) -> u64 {
        self.monthly
            .remaining(settings.relay_budget.map(|budget| budget.bytes))
    }

    pub(super) fn public_remaining(&self, settings: &Settings) -> u64 {
        self.public_daily
            .remaining(settings.contribution.daily_bytes)
    }

    pub(super) fn public_exhausted(&self, settings: &Settings, timestamp: i64) -> bool {
        match settings.contribution.daily_bytes {
            None => false,
            Some(0) => true,
            Some(_) => self.public_remaining(settings) == 0 && !self.public_day_elapsed(timestamp),
        }
    }

    pub(super) fn public_day_elapsed(&self, timestamp: i64) -> bool {
        self.public_daily.day_elapsed(timestamp)
    }

    pub(super) fn next_rollover(&self, settings: &Settings) -> i64 {
        let monthly = settings.relay_budget.and_then(|config| {
            self.monthly.next_rollover(Window::BillingMonth {
                reset_day: config.reset_day_utc,
            })
        });
        let daily = settings
            .contribution
            .daily_bytes
            .and_then(|_| self.public_daily.next_rollover(Window::PublicDay));
        match (monthly, daily) {
            (Some(a), Some(b)) => a.min(b),
            (Some(value), None) | (None, Some(value)) => value,
            (None, None) => i64::MAX,
        }
    }

    fn ready(&self, limits: Limits) -> u64 {
        self.monthly
            .ready(limits.monthly)
            .min(self.public_daily.ready(limits.public_daily))
    }

    const fn charge(&mut self, limits: Limits, written: u64) {
        self.monthly.spend(limits.monthly, written);
        self.public_daily.spend(limits.public_daily, written);
    }

    fn refill(&self, limits: Limits, requested: u64) -> BudgetRecords {
        let bytes = requested
            .min(self.monthly.remaining(limits.monthly))
            .min(self.public_daily.remaining(limits.public_daily));
        BudgetRecords {
            monthly: self.monthly.refill(limits.monthly, bytes),
            public_daily: self.public_daily.refill(limits.public_daily, bytes),
        }
    }

    const fn funded(&mut self, records: BudgetRecords) {
        self.monthly.funded(records.monthly);
        self.public_daily.funded(records.public_daily);
    }
}

#[cfg(test)]
#[path = "budget_tests.rs"]
mod tests;
