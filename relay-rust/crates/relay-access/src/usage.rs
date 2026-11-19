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

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Load {
    pub active_sessions: u64,
    pub wire_bytes: u64,
    pub forwarded_bytes: u64,
    pub admission_refusals: u64,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ShareAttribution {
    pub share: [u8; 12],
    pub account: AccountId,
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
    pub shares: Vec<ShareAttribution>,
    pub load: Load,
}

impl Batch {
    pub fn valid(&self) -> bool {
        self.id != [0; 16]
            && self.node != [0; 32]
            && self.issuer != [0; 32]
            && self.records.len() <= MAX_REPORT_RECORDS
            && self.resident_accounts.len() <= STATUS_BATCH_ENTRIES
            && self.shares.len() <= STATUS_BATCH_ENTRIES
            && (self.records.is_empty()
                || (self.resident_accounts.is_empty() && self.shares.is_empty()))
            && self
                .shares
                .iter()
                .all(|entry| entry.share != [0; 12] && entry.account != [0; 16])
            && self.records.iter().all(|record| {
                record.account != [0; 16]
                    && record.bytes > 0
                    && record.day <= self.created_at / crate::entitlement::DAY_SECONDS
            })
            && self
                .resident_accounts
                .iter()
                .all(|account| *account != [0; 16])
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AccountEntitlement {
    pub account: AccountId,
    pub entitlement: Entitlement,
}

/// Versioned independently of response time, including several changes in one second.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NodePolicy {
    pub version: u64,
    pub enabled: bool,
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
