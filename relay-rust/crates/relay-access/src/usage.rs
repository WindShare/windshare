//! Authority traffic records are transport-neutral and never contain content metadata.
use crate::{AccountId, Issuer, entitlement::Entitlement};
use alloc::{string::String, vec::Vec};
use serde::{Deserialize, Serialize};

pub const REPORT_INTERVAL_SECONDS: u64 = 60;
// Accept previously queued batches while keeping new batches within the wire limit
// even when all IDs, days and byte counts use their longest JSON representation.
const MAX_REPORT_RECORDS: usize = 4096;
pub const USAGE_BATCH_ENTRIES: usize = 2048;
pub const STATUS_BATCH_ENTRIES: usize = 1024;
pub const MAX_REPORT_BYTES: usize = 512 * 1024;
pub const USAGE_DOMAIN: &[u8] = b"windshare/node-usage/v1\0";
pub const RESPONSE_DOMAIN: &[u8] = b"windshare/authority-response/v2\0";

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct UsageRecord {
    pub account: AccountId,
    pub day: u64,
    pub bytes: u64,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum BudgetError {
    ResetDayOutOfRange,
    BytesOutOfRange,
}

impl core::fmt::Display for BudgetError {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        match self {
            Self::ResetDayOutOfRange => f.write_str("billing reset day must be 1..28 UTC"),
            Self::BytesOutOfRange => f.write_str("relay budget exceeds storage range"),
        }
    }
}

impl core::error::Error for BudgetError {}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct MonthlyBudget {
    pub bytes: u64,
    pub reset_day_utc: u8,
}

impl MonthlyBudget {
    pub const MIN_RESET_DAY_UTC: u8 = 1;
    pub const MAX_RESET_DAY_UTC: u8 = 28;
    // Matches the signed 64-bit SQLite ledger range on relay nodes.
    pub const MAX_BYTES: u64 = i64::MAX.cast_unsigned();

    /// # Errors
    /// Rejects reset days outside `1..=28` UTC and budgets above the signed 64-bit storage range.
    pub const fn validate(self) -> Result<(), BudgetError> {
        if self.reset_day_utc < Self::MIN_RESET_DAY_UTC
            || self.reset_day_utc > Self::MAX_RESET_DAY_UTC
        {
            return Err(BudgetError::ResetDayOutOfRange);
        }
        if self.bytes > Self::MAX_BYTES {
            return Err(BudgetError::BytesOutOfRange);
        }
        Ok(())
    }

    pub const fn valid(self) -> bool {
        self.validate().is_ok()
    }
}

/// Lightweight node-health counters piggybacked on authority usage reports.
///
/// Detailed node-local forwarding timing and throughput buckets remain in
/// per-node logs (`node_load_summary`) instead of expanding this signed wire
/// contract and single-row authority state.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Load {
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub relay_budget: Option<MonthlyBudget>,
    #[serde(default)]
    pub applied_policy_version: u64,
    #[serde(default)]
    pub resident_shares: u64,
    pub active_sessions: u64,
    pub wire_bytes: u64,
    pub forwarded_bytes: u64,
    pub admission_refusals: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Batch {
    pub id: [u8; 16],
    pub node: [u8; 32],
    pub issuer: Issuer,
    pub created_at: u64,
    pub records: Vec<UsageRecord>,
    pub resident_accounts: Vec<AccountId>,
    pub load: Load,
}

impl Batch {
    pub fn valid(&self) -> bool {
        self.id != [0; 16]
            && self.node != [0; 32]
            && self.issuer != [0; 32]
            && self.records.len() <= MAX_REPORT_RECORDS
            && self.resident_accounts.len() <= STATUS_BATCH_ENTRIES
            && (self.records.is_empty() || self.resident_accounts.is_empty())
            && self.records.iter().all(|record| {
                record.account != [0; 16]
                    && record.bytes > 0
                    && record.day <= self.created_at / crate::entitlement::DAY_SECONDS
            })
            && self
                .resident_accounts
                .iter()
                .all(|account| *account != [0; 16])
            && self.load.relay_budget.is_none_or(MonthlyBudget::valid)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AccountEntitlement {
    pub account: AccountId,
    pub entitlement: Entitlement,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Admission {
    Accepting,
    Draining,
    Disabled,
}

/// Versioned independently of response time, including several changes in one second.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NodePolicy {
    pub version: u64,
    pub admission: Admission,
    pub anonymous: bool,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Reply<T> {
    pub request: [u8; 16],
    pub node: [u8; 32],
    pub issuer: Issuer,
    pub generation: u64,
    pub issued_at: u64,
    pub result: T,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub enum ReportResult {
    UsageReceived,
    Status {
        entitlements: Vec<AccountEntitlement>,
        restriction_version: u64,
        policy: NodePolicy,
    },
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SignedReply {
    pub certificate: String,
    pub body: String,
}

#[cfg(test)]
#[path = "usage_tests.rs"]
mod tests;
