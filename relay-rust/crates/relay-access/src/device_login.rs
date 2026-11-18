//! Browser approval carries no reusable account credential across service origins.
use crate::{
    AccountId, ENROLLMENT_LIFETIME_MS, Issuer,
    trust::{self, Error, Generation},
};
use alloc::{string::String, vec::Vec};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use ed25519_dalek::SigningKey;
use serde::{Deserialize, Serialize, de::DeserializeOwned};

pub const CAPABILITY_HEADER: &str = "windshare-device-login";
pub const CAPABILITY_VERSION: &str = "1";
const INVITE_DOMAIN: &[u8] = b"windshare/device-invitation/v1\0";
const GRANT_DOMAIN: &[u8] = b"windshare/device-approval/v1\0";
const MAX_TOKEN_BYTES: usize = 4096;

#[derive(Debug, Serialize, Deserialize)]
pub struct Invitation {
    pub node: [u8; 32],
    pub issuer: Issuer,
    pub enrollment: [u8; 16],
    pub origin: String,
    pub device: String,
    pub code: String,
    pub expires_ms: u64,
}
#[derive(Debug, Serialize, Deserialize)]
pub struct Approval {
    pub node: [u8; 32],
    pub issuer: Issuer,
    pub enrollment: [u8; 16],
    pub account: AccountId,
    pub entitlement: crate::entitlement::Entitlement,
    pub expires_ms: u64,
}

fn issue<T: Serialize>(value: &T, domain: &[u8], signer: &SigningKey) -> Result<String, Error> {
    let bytes = serde_json::to_vec(value).map_err(|_| Error::Encoding)?;
    Ok(URL_SAFE_NO_PAD.encode(trust::sign(domain, &bytes, signer)))
}
fn bytes(token: &str) -> Result<Vec<u8>, Error> {
    if token.len() > MAX_TOKEN_BYTES {
        return Err(Error::Encoding);
    }
    URL_SAFE_NO_PAD.decode(token).map_err(|_| Error::Encoding)
}
fn decode<T: DeserializeOwned>(value: &[u8]) -> Result<T, Error> {
    serde_json::from_slice(value).map_err(|_| Error::Encoding)
}
const fn live(expiry: u64, now_ms: u64) -> Result<(), Error> {
    if expiry <= now_ms
        || expiry > now_ms.saturating_add(ENROLLMENT_LIFETIME_MS + trust::CLOCK_SKEW * 1000)
    {
        return Err(Error::Expired);
    }
    Ok(())
}
impl Invitation {
    /// # Errors
    /// The node signs the exact destination and device shown on the consent page.
    pub fn issue(&self, signer: &SigningKey) -> Result<String, Error> {
        issue(self, INVITE_DOMAIN, signer)
    }
    /// # Errors
    /// The authority must also check this node against its enrollment registry.
    pub fn verify(token: &str, issuer: Issuer, now_ms: u64) -> Result<Self, Error> {
        let signed = bytes(token)?;
        let (payload, _) = signed.split_last_chunk::<64>().ok_or(Error::Encoding)?;
        let value: Self = decode(payload)?;
        trust::verify(INVITE_DOMAIN, &signed, &value.node)?;
        if value.issuer != issuer {
            return Err(Error::Untrusted);
        }
        live(value.expires_ms, now_ms)?;
        Ok(value)
    }
}
impl Approval {
    /// # Errors
    /// Serialization failures prevent issuing a partially bound approval.
    pub fn issue(&self, signer: &SigningKey) -> Result<String, Error> {
        issue(self, GRANT_DOMAIN, signer)
    }
    /// # Errors
    /// Only the current issuer key may approve a pending enrollment at the named node.
    pub fn verify(
        token: &str,
        generation: Generation,
        node: [u8; 32],
        now_ms: u64,
    ) -> Result<Self, Error> {
        if !generation.usable(now_ms / 1000) {
            return Err(Error::Expired);
        }
        let signed = bytes(token)?;
        let value: Self = decode(trust::verify(GRANT_DOMAIN, &signed, &generation.key)?)?;
        if value.issuer != generation.issuer || value.node != node {
            return Err(Error::Untrusted);
        }
        live(value.expires_ms, now_ms)?;
        Ok(value)
    }
}
