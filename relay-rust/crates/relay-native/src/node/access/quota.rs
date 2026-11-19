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
    pub grant: Option<Grant>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Grant {
    pub valid_until: u64,
    pub remaining_bytes: u64,
}

impl Quota {
    pub(crate) fn from_snapshot(snapshot: Entitlement, now: u64) -> Self {
        Self {
            day: snapshot.day,
            version: snapshot.version,
            grant: Some(Grant {
                valid_until: snapshot.valid_until,
                remaining_bytes: snapshot.remaining_bytes,
            }),
        }
        .at(now)
    }

    fn at(mut self, now: u64) -> Self {
        if self.grant.is_some_and(|grant| now >= grant.valid_until) {
            self.grant = None;
        }
        self
    }

    fn deadline(self) -> u128 {
        // Earlier versions can have different expiry times. All snapshots for
        // this UTC day become unusable at its end, regardless of their version.
        self.grant.map_or_else(
            || (u128::from(self.day) + 1) * u128::from(DAY_SECONDS),
            |grant| u128::from(grant.valid_until),
        )
    }

    fn allowance(self, now: u64) -> Allowance {
        self.grant.map_or(Allowance::Stale, |grant| {
            Entitlement {
                day: self.day,
                version: self.version,
                valid_until: grant.valid_until,
                remaining_bytes: grant.remaining_bytes,
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
            if previous.day >= day_floor
                && (next.version <= previous.version || next.day < previous.day)
            {
                return false;
            }
            self.deadlines.remove(&(previous.deadline(), key));
        }
        self.insert(key, next.at(now));
        true
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
