use crate::{AccountId, Attribution, CredentialId, Issuer};
use alloc::collections::BTreeSet;
use serde::{Deserialize, Serialize};

/// A complete, monotonic access snapshot. No clock or allowance reset clears it.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Restrictions {
    pub version: u64,
    pub shares: BTreeSet<[u8; 12]>,
    pub accounts: BTreeSet<AccountId>,
    pub credentials: BTreeSet<CredentialId>,
}

impl Restrictions {
    pub fn denied(&self, issuer: Issuer, share: [u8; 12], attribution: Attribution) -> bool {
        self.shares.contains(&share)
            || matches!(attribution,
            Attribution::Account { issuer: actual, account, credential }
                if actual == issuer && (self.accounts.contains(&account)
                    || self.credentials.contains(&credential)))
    }

    pub fn advance(&mut self, next: Self) -> bool {
        if next.version <= self.version {
            return false;
        }
        *self = next;
        true
    }
}

#[cfg(test)]
#[path = "restrictions_tests.rs"]
mod tests;
