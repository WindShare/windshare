use crate::{Fault, registration::digest};
use anyhow::{Result, bail, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{collections::BTreeSet, fmt, sync::Arc};

pub const PASSWORD_SALT_BYTES: usize = 16;
pub const PASSWORD_VERIFIER_BYTES: usize = 32;
pub const PASSWORD_PEPPER_BYTES: usize = 32;

const PASSWORD_SALT_ENCODED_LEN: usize = 22;
const PASSWORD_VERIFIER_ENCODED_LEN: usize = 43;
const PASSWORD_PEPPER_ENCODED_LEN: usize = 43;
const MAX_PEPPER_ID_LEN: usize = 16;
const HMAC_BLOCK_BYTES: usize = 64;
const STORED_PASSWORD_MAC_DOMAIN: &[u8] = b"windshare/authority-password-mac/v1\0";

pub fn invalid_credentials() -> Fault {
    Fault::Denied {
        reason: "credentials".into(),
    }
}

fn valid_pepper_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= MAX_PEPPER_ID_LEN
        && id.bytes().all(|byte| {
            byte.is_ascii_lowercase() || byte.is_ascii_digit() || matches!(byte, b'_' | b'-')
        })
}

fn decode_fixed<const N: usize>(encoded: &str, expected_len: usize) -> Option<[u8; N]> {
    if encoded.len() != expected_len {
        return None;
    }
    let mut output = [0_u8; N];
    let decoded = URL_SAFE_NO_PAD.decode_slice(encoded, &mut output).ok()?;
    (decoded == N).then_some(output)
}

pub fn hmac_sha256(key: &[u8; PASSWORD_PEPPER_BYTES], parts: &[&[u8]]) -> [u8; 32] {
    let mut ipad = [0x36_u8; HMAC_BLOCK_BYTES];
    let mut opad = [0x5c_u8; HMAC_BLOCK_BYTES];
    for (index, &byte) in key.iter().enumerate() {
        ipad[index] ^= byte;
        opad[index] ^= byte;
    }
    let mut inner = Sha256::new();
    inner.update(ipad);
    for part in parts {
        inner.update(part);
    }
    let inner_hash = inner.finalize();
    let mut outer = Sha256::new();
    outer.update(opad);
    outer.update(inner_hash);
    outer.finalize().into()
}

fn constant_time_eq(left: &[u8; 32], right: &[u8; 32]) -> bool {
    let mut diff = 0_u8;
    for (&a, &b) in left.iter().zip(right.iter()) {
        diff |= a ^ b;
    }
    std::hint::black_box(diff) == 0
}

#[derive(Clone, PartialEq, Eq)]
struct PasswordPepper {
    id: String,
    key: [u8; PASSWORD_PEPPER_BYTES],
}

impl fmt::Debug for PasswordPepper {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("PasswordPepper")
            .field("id", &self.id)
            .finish_non_exhaustive()
    }
}

/// Ordered set of server-side pepper secrets parsed from `PASSWORD_PEPPER`.
///
/// The first entry seals new password records while all entries verify existing
/// records so operators can rotate peppers without locking out active accounts.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PepperRing(Arc<[PasswordPepper]>);

impl PepperRing {
    /// Parses a comma-separated list of `<id>:<base64url-32-bytes>` pepper entries.
    ///
    /// # Errors
    /// Rejects empty rings, malformed IDs, duplicate IDs, non-canonical base64url keys, or all-zero keys.
    pub fn parse(raw: &str) -> Result<Self> {
        let trimmed = raw.trim();
        ensure!(!trimmed.is_empty(), "PASSWORD_PEPPER must not be empty");
        let mut seen = BTreeSet::new();
        let mut entries = Vec::new();
        for part in trimmed.split(',') {
            let entry = part.trim();
            let Some((id, encoded_key)) = entry.split_once(':') else {
                bail!("PASSWORD_PEPPER entry must use <id>:<base64url> format");
            };
            ensure!(valid_pepper_id(id), "invalid PASSWORD_PEPPER id: {id}");
            ensure!(
                seen.insert(id.to_owned()),
                "duplicate PASSWORD_PEPPER id: {id}"
            );
            let Some(key) =
                decode_fixed::<PASSWORD_PEPPER_BYTES>(encoded_key, PASSWORD_PEPPER_ENCODED_LEN)
            else {
                bail!("PASSWORD_PEPPER key for {id} must be 32-byte unpadded base64url");
            };
            ensure!(
                key != [0_u8; PASSWORD_PEPPER_BYTES],
                "PASSWORD_PEPPER key for {id} must not be all zeros"
            );
            entries.push(PasswordPepper {
                id: id.to_owned(),
                key,
            });
        }
        Ok(Self(Arc::from(entries)))
    }

    pub fn active_id(&self) -> &str {
        &self.0[0].id
    }

    pub fn len(&self) -> usize {
        self.0.len()
    }

    pub fn is_empty(&self) -> bool {
        self.0.is_empty()
    }

    fn active(&self) -> &PasswordPepper {
        &self.0[0]
    }

    fn find(&self, id: &str) -> Option<&PasswordPepper> {
        self.0.iter().find(|entry| entry.id == id)
    }
}

/// Client-derived 32-byte PBKDF2 password verifier sent in place of plaintext passwords.
#[derive(Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(try_from = "String", into = "String")]
pub struct PasswordVerifier([u8; PASSWORD_VERIFIER_BYTES]);

impl PasswordVerifier {
    pub const fn from_bytes(bytes: [u8; PASSWORD_VERIFIER_BYTES]) -> Self {
        Self(bytes)
    }

    pub const fn as_bytes(&self) -> &[u8; PASSWORD_VERIFIER_BYTES] {
        &self.0
    }
}

impl fmt::Debug for PasswordVerifier {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("PasswordVerifier([REDACTED])")
    }
}

impl TryFrom<String> for PasswordVerifier {
    type Error = Fault;

    fn try_from(raw: String) -> std::result::Result<Self, Self::Error> {
        decode_fixed::<PASSWORD_VERIFIER_BYTES>(&raw, PASSWORD_VERIFIER_ENCODED_LEN)
            .map(Self)
            .ok_or(Fault::Invalid)
    }
}

impl From<PasswordVerifier> for String {
    fn from(value: PasswordVerifier) -> Self {
        URL_SAFE_NO_PAD.encode(value.0)
    }
}

/// Pepper-keyed HMAC-SHA256 record stored in Durable Object SQLite.
///
/// Serialized as `<pepper_id>$<salt_b64>$<tag_b64>` so every password write
/// binds a fresh random salt and rotates its fingerprint even on A -> B -> A changes.
#[derive(Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(try_from = "String", into = "String")]
pub struct StoredPassword {
    pepper_id: String,
    salt: [u8; PASSWORD_SALT_BYTES],
    tag: [u8; 32],
}

impl StoredPassword {
    pub fn seal(
        verifier: &PasswordVerifier,
        salt: [u8; PASSWORD_SALT_BYTES],
        peppers: &PepperRing,
    ) -> Self {
        let active = peppers.active();
        let tag = hmac_sha256(
            &active.key,
            &[STORED_PASSWORD_MAC_DOMAIN, &salt, verifier.as_bytes()],
        );
        Self {
            pepper_id: active.id.clone(),
            salt,
            tag,
        }
    }

    pub fn verify(&self, verifier: &PasswordVerifier, peppers: &PepperRing) -> bool {
        let Some(pepper) = peppers.find(&self.pepper_id) else {
            tracing::warn!(
                event = "password_pepper_missing",
                pepper_id = %self.pepper_id
            );
            return false;
        };
        let expected = hmac_sha256(
            &pepper.key,
            &[STORED_PASSWORD_MAC_DOMAIN, &self.salt, verifier.as_bytes()],
        );
        constant_time_eq(&self.tag, &expected)
    }

    pub fn fingerprint(&self) -> [u8; 32] {
        digest(&self.encode())
    }

    pub fn pepper_id(&self) -> &str {
        &self.pepper_id
    }

    fn encode(&self) -> String {
        format!(
            "{}${}${}",
            self.pepper_id,
            URL_SAFE_NO_PAD.encode(self.salt),
            URL_SAFE_NO_PAD.encode(self.tag),
        )
    }
}

impl fmt::Debug for StoredPassword {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("StoredPassword")
            .field("pepper_id", &self.pepper_id)
            .finish_non_exhaustive()
    }
}

impl TryFrom<String> for StoredPassword {
    type Error = Fault;

    fn try_from(raw: String) -> std::result::Result<Self, Self::Error> {
        let mut parts = raw.split('$');
        let (Some(pepper_id), Some(salt_str), Some(tag_str), None) =
            (parts.next(), parts.next(), parts.next(), parts.next())
        else {
            return Err(Fault::Invalid);
        };
        if !valid_pepper_id(pepper_id) {
            return Err(Fault::Invalid);
        }
        let salt = decode_fixed::<PASSWORD_SALT_BYTES>(salt_str, PASSWORD_SALT_ENCODED_LEN)
            .ok_or(Fault::Invalid)?;
        let tag =
            decode_fixed::<32>(tag_str, PASSWORD_VERIFIER_ENCODED_LEN).ok_or(Fault::Invalid)?;
        Ok(Self {
            pepper_id: pepper_id.to_owned(),
            salt,
            tag,
        })
    }
}

impl From<StoredPassword> for String {
    fn from(value: StoredPassword) -> Self {
        value.encode()
    }
}
