use std::collections::{BTreeMap, BTreeSet};
use windshare_relay_access::{
    AccountId, Issuer,
    entitlement::{Allowance, DAY_SECONDS, Entitlement},
};

pub type AccountKey = (Issuer, AccountId);

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Quota {
    pub day: u64,
    pub version: u64,
    pub valid_until: u64,
    pub remaining_bytes: Option<u64>,
}

impl Quota {
    pub(crate) const fn from_snapshot(snapshot: Entitlement, now: u64) -> Self {
        Self {
            day: snapshot.day,
            version: snapshot.version,
            valid_until: snapshot.valid_until,
            remaining_bytes: Some(snapshot.remaining_bytes),
        }
        .at(now)
    }

    const fn order_key(self) -> (u64, u64, u64) {
        (self.day, self.version, self.valid_until)
    }

    const fn at(mut self, now: u64) -> Self {
        // Keep `valid_until` as part of the monotonic watermark after expiry so
        // replaying an old snapshot cannot revive allowance even under clock rollback.
        if now >= self.valid_until {
            self.remaining_bytes = None;
        }
        self
    }

    fn deadline(self) -> u128 {
        // Active grants retire when `valid_until` passes; expired watermarks stay
        // until the end of the UTC day so older tickets cannot roll them back.
        if self.remaining_bytes.is_some() {
            u128::from(self.valid_until)
        } else {
            (u128::from(self.day) + 1) * u128::from(DAY_SECONDS)
        }
    }

    fn allowance(self, now: u64) -> Allowance {
        self.remaining_bytes
            .map_or(Allowance::Stale, |remaining_bytes| {
                Entitlement {
                    day: self.day,
                    version: self.version,
                    valid_until: self.valid_until,
                    remaining_bytes,
                }
                .allowance(now)
            })
    }
}

#[derive(Default)]
pub(super) struct Quotas {
    accounts: BTreeMap<AccountKey, Quota>,
    deadlines: BTreeSet<(u128, AccountKey)>,
    day_floor: u64,
}

impl Quotas {
    pub(super) fn allowance(&self, key: AccountKey, now: u64) -> Allowance {
        self.accounts
            .get(&key)
            .filter(|quota| quota.day >= self.day_floor)
            .map_or(Allowance::Stale, |quota| quota.allowance(now))
    }

    pub(super) fn advance(&mut self, key: AccountKey, next: Quota, now: u64) -> bool {
        let day_floor = self.day_floor.max(now / DAY_SECONDS);
        if next.day < day_floor {
            return false;
        }
        let previous = self.accounts.get(&key).copied();
        if let Some(previous) = previous {
            if previous.day >= day_floor && next.order_key() <= previous.order_key() {
                return false;
            }
            self.deadlines.remove(&(previous.deadline(), key));
        }
        self.insert(key, next.at(now));
        true
    }

    pub(super) fn needs_retire(&self, day: u64, now: u64) -> bool {
        let day_floor = self.day_floor.max(day).max(now / DAY_SECONDS);
        let effective_now = now.max(day_floor.saturating_mul(DAY_SECONDS));
        day_floor > self.day_floor
            || self
                .deadlines
                .first()
                .is_some_and(|&(deadline, _)| deadline <= u128::from(effective_now))
    }

    pub(super) fn retire_before(&mut self, day: u64, now: u64) -> usize {
        self.day_floor = self.day_floor.max(day).max(now / DAY_SECONDS);
        let now = now.max(self.day_floor.saturating_mul(DAY_SECONDS));
        let mut retired = 0;
        while let Some((deadline, key)) = self.deadlines.first().copied() {
            if deadline > u128::from(now) {
                break;
            }
            self.deadlines.pop_first();
            let quota = self.accounts.remove(&key).expect("indexed quota");
            if quota.day >= self.day_floor {
                self.insert(key, quota.at(now));
            }
            retired += 1;
        }
        retired
    }

    fn insert(&mut self, key: AccountKey, quota: Quota) {
        self.accounts.insert(key, quota);
        self.deadlines.insert((quota.deadline(), key));
    }
}

#[cfg(test)]
#[path = "quota_tests.rs"]
mod tests;
