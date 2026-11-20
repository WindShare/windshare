use crate::{Fault, Session, registration::digest};
use anyhow::{Result, ensure};
use argon2::{
    Algorithm, Argon2, Params, Version,
    password_hash::{PasswordHash, PasswordHasher, PasswordVerifier, SaltString},
};
use serde::{Deserialize, Serialize};
use windshare_relay_access::AccountId;

const MIN_PASSWORD: usize = 12;
const MAX_PASSWORD: usize = 256;
const MAX_PASSWORD_HASH_BYTES: usize = 512;
pub const PASSWORD_SALT_BYTES: usize = 16;

// OWASP Argon2id baseline tuned for 128 MiB Cloudflare Workers isolates:
// 12 MiB memory with 3 iterations preserves the OWASP 19 MiB / 2-iteration
// work factor while keeping WASM linear memory growth bounded.
const PASSWORD_MEMORY_KIB: u32 = 12_288;
const PASSWORD_ITERATIONS: u32 = 3;
const PASSWORD_PARALLELISM: u32 = 1;

pub const fn valid_password_length(len: usize) -> bool {
    len >= MIN_PASSWORD && len <= MAX_PASSWORD
}

pub const fn valid_login_password_length(len: usize) -> bool {
    len <= MAX_PASSWORD
}

pub fn invalid_credentials() -> Fault {
    Fault::Denied {
        reason: "credentials".into(),
    }
}

fn password_argon2() -> Result<Argon2<'static>> {
    let params = Params::new(
        PASSWORD_MEMORY_KIB,
        PASSWORD_ITERATIONS,
        PASSWORD_PARALLELISM,
        None,
    )
    .map_err(|_| Fault::Invalid)?;
    Ok(Argon2::new(Algorithm::Argon2id, Version::V0x13, params))
}

/// Validated Argon2id PHC string so plaintext passwords cannot cross into the
/// Durable Object state machine or storage records.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(try_from = "String", into = "String")]
pub struct HashedPassword(String);

impl HashedPassword {
    /// # Errors
    /// Rejects passwords outside the allowed byte length range or invalid salt encoding.
    pub fn hash(password: &str, salt: &[u8; PASSWORD_SALT_BYTES]) -> Result<Self> {
        ensure!(valid_password_length(password.len()), Fault::Invalid);
        let salt = SaltString::encode_b64(salt).map_err(|_| Fault::Invalid)?;
        let encoded = password_argon2()?
            .hash_password(password.as_bytes(), &salt)
            .map(|hash| hash.to_string())
            .map_err(|_| Fault::Invalid)?;
        Ok(Self(encoded))
    }

    pub fn verify(&self, password: &str) -> bool {
        valid_login_password_length(password.len())
            && PasswordHash::new(&self.0).is_ok_and(|hash| {
                Argon2::default()
                    .verify_password(password.as_bytes(), &hash)
                    .is_ok()
            })
    }

    pub fn fingerprint(&self) -> [u8; 32] {
        digest(&self.0)
    }

    pub const fn as_str(&self) -> &str {
        self.0.as_str()
    }
}

impl TryFrom<String> for HashedPassword {
    type Error = Fault;

    fn try_from(raw: String) -> std::result::Result<Self, Self::Error> {
        if raw.is_empty() || raw.len() > MAX_PASSWORD_HASH_BYTES {
            return Err(Fault::Invalid);
        }
        let parsed = PasswordHash::new(&raw).map_err(|_| Fault::Invalid)?;
        if parsed.algorithm != Algorithm::Argon2id.ident()
            || parsed.salt.is_none()
            || parsed.hash.is_none()
            || Params::try_from(&parsed).is_err()
        {
            return Err(Fault::Invalid);
        }
        Ok(Self(raw))
    }
}

impl From<HashedPassword> for String {
    fn from(value: HashedPassword) -> Self {
        value.0
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct PasswordChallenge {
    pub account: AccountId,
    pub password: HashedPassword,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct PasswordChangePreparation {
    pub previous_verifier_digest: Option<[u8; 32]>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PasswordRegistrationPrepare {
    pub client_secret: String,
    pub challenge: Option<String>,
    pub solution: Option<String>,
    pub username: String,
    pub password_len: usize,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "status", rename_all = "snake_case")]
pub enum PasswordRegistrationPreparation {
    Existing { session: Box<Session> },
    Ready,
}
