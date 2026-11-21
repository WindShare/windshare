use crate::Failure;
use serde::{Deserialize, Serialize};
use std::time::{Duration, SystemTime};

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct AccountView {
    pub status: AccountStatus,
    #[serde(default)]
    pub reset_local: String,
    #[serde(default)]
    pub nickname_change_local: String,
    pub operation: Option<AccountOperation>,
}

impl AccountView {
    pub fn nickname_deadline(&self) -> Option<SystemTime> {
        let nickname = self.status.profile.nickname.as_ref()?;
        SystemTime::UNIX_EPOCH.checked_add(Duration::from_secs(nickname.next_change_at))
    }

    pub fn nickname_locked_until(&self) -> Option<&str> {
        self.nickname_deadline()
            .filter(|at| *at > SystemTime::now())
            .map(|_| self.nickname_change_local.as_str())
    }

    pub const fn can_edit_profile(&self) -> bool {
        !self.status.account.is_empty()
            && !matches!(
                self.status.state,
                AccountState::Denied | AccountState::StorageError
            )
    }

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
    #[serde(flatten)]
    pub state: AccountState,
    #[serde(default)]
    pub email_code_pending: bool,
    #[serde(default)]
    pub account: String,
    #[serde(default)]
    pub credential: String,
    #[serde(default)]
    pub username: String,
    #[serde(default)]
    pub profile: AccountProfile,
    pub pending_email: Option<PendingEmail>,
    pub allowance: Allowance,
    pub remaining_bytes: u64,
    #[serde(default)]
    pub reset_at: String,
    #[serde(default)]
    pub reason: String,
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Eq)]
pub struct AccountProfile {
    pub nickname: Option<Nickname>,
    #[serde(default)]
    pub email: String,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct Nickname {
    pub value: String,
    pub next_change_at: u64,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct PendingEmail {
    pub address: String,
    pub challenge: EmailChallenge,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct EmailChallenge {
    pub id: String,
}

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(tag = "state", rename_all = "snake_case")]
pub enum AccountState {
    PendingSetup,
    RegistrationRequired,
    Registering {
        #[serde(default)]
        registration_permille: u16,
    },
    EmailRequired,
    Ready,
    RefreshRequired,
    Refreshing,
    TemporarilyUnavailable,
    StorageError,
    Denied,
}
#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Allowance {
    Available,
    Spent,
    Stale,
}

#[derive(Serialize)]
#[serde(tag = "action", rename_all = "snake_case")]
pub enum AccountCommand {
    Register,
    Refresh,
    Login {
        username: String,
        password: String,
    },
    Password {
        account_id: String,
        username: String,
        password: String,
    },
    Nickname {
        account_id: String,
        nickname: String,
    },
    RequestProfileEmail {
        account_id: String,
        email: String,
    },
    ConfirmProfileEmail {
        account_id: String,
        challenge: String,
        code: String,
    },
    Email {
        email: String,
    },
    Verify {
        code: String,
    },
    Revoke {
        account_id: String,
        credential: String,
    },
}

impl AccountCommand {
    pub(crate) const fn action(&self) -> &'static str {
        match self {
            Self::Register => "register",
            Self::Refresh => "refresh",
            Self::Login { .. } => "login",
            Self::Password { .. } => "password",
            Self::Nickname { .. } => "nickname",
            Self::RequestProfileEmail { .. } => "request_profile_email",
            Self::ConfirmProfileEmail { .. } => "confirm_profile_email",
            Self::Email { .. } => "email",
            Self::Verify { .. } => "verify",
            Self::Revoke { .. } => "revoke",
        }
    }
}
