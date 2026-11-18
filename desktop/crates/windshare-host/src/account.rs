use crate::Failure;
use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct AccountView {
    pub status: AccountStatus,
    pub reset_local: String,
    pub operation: Option<AccountOperation>,
}

impl AccountView {
    pub const fn busy(&self) -> bool {
        matches!(
            self.operation,
            Some(AccountOperation {
                outcome: AccountOutcome::Running,
                ..
            })
        )
    }
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct AccountOperation {
    pub id: String,
    #[serde(flatten)]
    pub outcome: AccountOutcome,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
#[serde(tag = "state", rename_all = "snake_case")]
pub enum AccountOutcome {
    Running,
    Succeeded,
    Failed { error: Failure },
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct AccountStatus {
    pub active: bool,
    pub state: AccountState,
    #[serde(default)]
    pub email_code_pending: bool,
    #[serde(default)]
    pub account: String,
    #[serde(default)]
    pub credential: String,
    #[serde(default)]
    pub username: String,
    pub allowance: Allowance,
    pub remaining_bytes: u64,
    pub reset_at: String,
    #[serde(default)]
    pub hashes: u64,
    #[serde(default)]
    pub proof_elapsed: u64,
    #[serde(default)]
    pub reason: String,
}

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum AccountState {
    PendingSetup,
    ProofOfWork,
    EmailRequired,
    Ready,
    Refreshing,
    TemporarilyUnavailable,
    Denied,
}
#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Allowance {
    Available,
    Spent,
    Stale,
}

#[derive(Debug, Serialize)]
#[serde(tag = "action", rename_all = "snake_case")]
pub enum AccountCommand {
    Retry,
    Login { username: String, password: String },
    Password { username: String, password: String },
    Email { email: String },
    Verify { code: String },
    Revoke { credential: String },
}
