use crate::{PasswordVerifier, StoredPassword};
use serde::{Deserialize, Deserializer, Serialize};
use windshare_relay_access::{
    AccountId, CredentialId,
    entitlement::{DEFAULT_DAILY_BYTES, Entitlement},
};

// Approximately 16 seconds at the native client's measured 16M attempts/s.
const DEFAULT_POW_BITS: u8 = 28;
const AUTOMATIC_ACCOUNT_LIMIT: u64 = 1_000;
const WORK_ACCOUNT_LIMIT: u64 = 4_000;

#[derive(Clone, Copy, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum RegistrationMode {
    Automatic,
    ProofOfWork,
    Email,
}

#[derive(Clone, Copy, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum RegistrationPolicy {
    ByAccountCount,
    Automatic,
    ProofOfWork,
    Email,
}

impl RegistrationPolicy {
    pub(crate) const fn is_milestone(count: u64) -> bool {
        matches!(count, AUTOMATIC_ACCOUNT_LIMIT | WORK_ACCOUNT_LIMIT)
    }

    pub(crate) const fn mode(self, account_count: u64, email_configured: bool) -> RegistrationMode {
        match self {
            Self::ByAccountCount if account_count < AUTOMATIC_ACCOUNT_LIMIT => {
                RegistrationMode::Automatic
            }
            Self::ByAccountCount if account_count >= WORK_ACCOUNT_LIMIT && email_configured => {
                RegistrationMode::Email
            }
            // A deployment without email can still accept new accounts at the final milestone.
            Self::ByAccountCount | Self::ProofOfWork => RegistrationMode::ProofOfWork,
            Self::Automatic => RegistrationMode::Automatic,
            Self::Email => RegistrationMode::Email,
        }
    }
}

#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct Rate {
    pub burst: u64,
    pub refill_seconds: u64,
}
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct Settings {
    pub registration: RegistrationPolicy,
    pub pow_bits: u8,
    pub daily_bytes: u64,
    pub source_rate: Rate,
    pub global_rate: Rate,
}
impl Default for Settings {
    fn default() -> Self {
        Self {
            registration: RegistrationPolicy::ByAccountCount,
            pow_bits: DEFAULT_POW_BITS,
            daily_bytes: DEFAULT_DAILY_BYTES,
            source_rate: Rate {
                burst: 20,
                refill_seconds: 600,
            },
            global_rate: Rate {
                burst: 1000,
                refill_seconds: 2,
            },
        }
    }
}

#[derive(Debug, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct SettingsChanges {
    #[serde(deserialize_with = "specified")]
    pub registration: Option<RegistrationPolicy>,
    #[serde(deserialize_with = "specified")]
    pub pow_bits: Option<u8>,
    #[serde(deserialize_with = "specified")]
    pub daily_bytes: Option<u64>,
    pub source_rate: RateChanges,
    pub global_rate: RateChanges,
}

impl SettingsChanges {
    pub(crate) fn apply_to(&self, current: &Settings) -> Settings {
        Settings {
            registration: self.registration.unwrap_or(current.registration),
            pow_bits: self.pow_bits.unwrap_or(current.pow_bits),
            daily_bytes: self.daily_bytes.unwrap_or(current.daily_bytes),
            source_rate: self.source_rate.apply_to(&current.source_rate),
            global_rate: self.global_rate.apply_to(&current.global_rate),
        }
    }
}

#[derive(Debug, Default, Deserialize)]
#[serde(default, deny_unknown_fields)]
pub struct RateChanges {
    #[serde(deserialize_with = "specified")]
    pub burst: Option<u64>,
    #[serde(deserialize_with = "specified")]
    pub refill_seconds: Option<u64>,
}

impl RateChanges {
    fn apply_to(&self, current: &Rate) -> Rate {
        Rate {
            burst: self.burst.unwrap_or(current.burst),
            refill_seconds: self.refill_seconds.unwrap_or(current.refill_seconds),
        }
    }
}

// Stored settings are required, so a request can omit fields but cannot clear them.
fn specified<'de, D: Deserializer<'de>, T: Deserialize<'de>>(
    deserializer: D,
) -> Result<Option<T>, D::Error> {
    T::deserialize(deserializer).map(Some)
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Account {
    pub id: AccountId,
    pub created_at: u64,
    pub username: Option<String>,
    pub password: Option<StoredPassword>,
    #[serde(default)]
    pub profile: Profile,
}
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct Profile {
    pub revision: u64,
    pub nickname: Option<Nickname>,
    pub email: Option<String>,
}
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Nickname {
    pub value: String,
    pub next_change_at: u64,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Credential {
    pub id: CredentialId,
    pub account: AccountId,
    pub secret_digest: [u8; 32],
    pub created_at: u64,
    pub revoked: bool,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Challenge {
    pub id: String,
    pub mode: RegistrationMode,
    pub bits: u8,
    pub expires_at: u64,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Registration {
    pub client_secret: String,
    pub challenge: Option<String>,
    pub solution: Option<String>,
    pub username: Option<String>,
    pub password_verifier: Option<PasswordVerifier>,
}
#[derive(Debug, Serialize, Deserialize)]
pub struct Session {
    pub account: AccountId,
    pub credential: CredentialId,
    pub credential_secret: Option<String>,
    pub username: Option<String>,
    pub profile: Profile,
    pub ticket: String,
    pub expires_at: u64,
    pub entitlement: Entitlement,
    pub certificate: String,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize)]
#[serde(tag = "reason")]
pub enum DiscoveryFailure {
    #[serde(rename = "invalid_node_url")]
    InvalidUrl,
    #[serde(rename = "reserved_node_host")]
    ReservedHost,
    #[serde(rename = "node_dns_failed")]
    DnsFailed,
    #[serde(rename = "node_non_public_address")]
    NonPublicAddress,
    #[serde(rename = "node_unreachable")]
    Unreachable,
    #[serde(rename = "node_redirect_refused")]
    RedirectRefused,
    #[serde(rename = "node_http_status")]
    HttpStatus { status: u16 },
    #[serde(rename = "node_invalid_descriptor")]
    InvalidDescriptor,
    #[serde(rename = "node_descriptor_expired")]
    DescriptorExpired,
    #[serde(rename = "node_descriptor_mismatch")]
    DescriptorMismatch,
}
impl DiscoveryFailure {
    pub const fn http_status(self) -> u16 {
        match self {
            Self::InvalidUrl | Self::ReservedHost => 400,
            Self::NonPublicAddress
            | Self::InvalidDescriptor
            | Self::DescriptorExpired
            | Self::DescriptorMismatch => 422,
            Self::DnsFailed
            | Self::Unreachable
            | Self::RedirectRefused
            | Self::HttpStatus { .. } => 502,
        }
    }

    pub const fn reason(self) -> &'static str {
        match self {
            Self::InvalidUrl => "invalid_node_url",
            Self::ReservedHost => "reserved_node_host",
            Self::DnsFailed => "node_dns_failed",
            Self::NonPublicAddress => "node_non_public_address",
            Self::Unreachable => "node_unreachable",
            Self::RedirectRefused => "node_redirect_refused",
            Self::HttpStatus { .. } => "node_http_status",
            Self::InvalidDescriptor => "node_invalid_descriptor",
            Self::DescriptorExpired => "node_descriptor_expired",
            Self::DescriptorMismatch => "node_descriptor_mismatch",
        }
    }
}
#[derive(Debug, Serialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Fault {
    Invalid,
    ReportNotFound,
    NodeNotFound,
    AccountNotFound,
    InvalidNickname,
    UnsupportedEmail,
    InvalidEmail,
    EmailCodeRejected,
    EmailDeliveryUnavailable,
    NicknameCooldown {
        profile: Profile,
    },
    Conflict {
        reason: String,
    },
    Denied {
        reason: String,
    },
    NodeDiscovery {
        #[serde(flatten)]
        failure: DiscoveryFailure,
    },
    RateLimited {
        retry_after: u64,
    },
    Verification {
        challenge: Challenge,
    },
}
impl Fault {
    pub(crate) fn administrator_session_required() -> Self {
        Self::Denied {
            reason: "administrator_session".into(),
        }
    }

    pub const fn node_discovery(failure: DiscoveryFailure) -> Self {
        Self::NodeDiscovery { failure }
    }
}
impl std::fmt::Display for Fault {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}
impl std::error::Error for Fault {}
