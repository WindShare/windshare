use serde::{Deserialize, Serialize};
use windshare_relay_access::{
    AccountId, CredentialId,
    entitlement::{DEFAULT_DAILY_BYTES, Entitlement},
};

// Approximately 16 seconds at the native client's measured 16M attempts/s.
const DEFAULT_POW_BITS: u8 = 28;

#[derive(Clone, Copy, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum RegistrationMode {
    Automatic,
    ProofOfWork,
    Email,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Rate {
    pub burst: u64,
    pub refill_seconds: u64,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Settings {
    pub registration: RegistrationMode,
    pub pow_bits: u8,
    pub daily_bytes: u64,
    pub source_rate: Rate,
    pub global_rate: Rate,
}
impl Default for Settings {
    fn default() -> Self {
        Self {
            registration: RegistrationMode::Automatic,
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

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Account {
    pub id: AccountId,
    pub created_at: u64,
    pub username: Option<String>,
    pub password: Option<String>,
    pub version: u64,
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
pub struct Node {
    #[serde(default)]
    pub version: u64,
    pub key: [u8; 32],
    pub zone: String,
    pub region: String,
    pub anonymous: bool,
    pub enabled: bool,
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
    pub password: Option<String>,
}
#[derive(Debug, Serialize, Deserialize)]
pub struct Session {
    pub account: AccountId,
    pub credential: CredentialId,
    pub credential_secret: Option<String>,
    pub username: Option<String>,
    pub ticket: String,
    pub expires_at: u64,
    pub entitlement: Entitlement,
    pub certificate: String,
}
#[derive(Debug, Serialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Fault {
    Invalid,
    Denied { reason: String },
    RateLimited { retry_after: u64 },
    Verification { challenge: Challenge },
}
impl std::fmt::Display for Fault {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}
impl std::error::Error for Fault {}
