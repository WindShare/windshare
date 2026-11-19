use crate::{AccountId, Attribution, CredentialId, Issuer};
use alloc::{collections::BTreeSet, vec::Vec};
use serde::{Deserialize, Serialize};

pub const PAGE_ENTRIES: usize = 1024;
pub const REQUEST_DOMAIN: &[u8] = b"windshare/node-restrictions/v1\0";
pub const RESPONSE_DOMAIN: &[u8] = b"windshare/restriction-page/v1\0";

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(
    tag = "kind",
    content = "id",
    rename_all = "snake_case",
    deny_unknown_fields
)]
pub enum Restriction {
    Account(AccountId),
    Credential(CredentialId),
    Share([u8; 12]),
}

#[derive(Clone, Copy, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PageRequest {
    pub id: [u8; 16],
    pub node: [u8; 32],
    pub issuer: Issuer,
    pub version: u64,
    pub after: Option<Restriction>,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub enum PageResult {
    Superseded {
        version: u64,
    },
    Page {
        entries: Vec<Restriction>,
        more: bool,
    },
}

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
    pub fn insert(&mut self, entry: Restriction) -> bool {
        match entry {
            Restriction::Account(id) => self.accounts.insert(id),
            Restriction::Credential(id) => self.credentials.insert(id),
            Restriction::Share(id) => self.shares.insert(id),
        }
    }

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
