use crate::{
    AccountId, Attribution, CredentialId, Issuer,
    cbor::{Decoder, Encoder},
    entitlement::Entitlement,
    trust::{self, Error, Generation, TrustStore},
};
use alloc::vec::Vec;
use ed25519_dalek::SigningKey;
use serde::{Deserialize, Serialize};

pub const TICKET_LIFETIME: u64 = 12 * 60 * 60;
pub const MAX_TICKET_BYTES: usize = 512;
const TICKET_DOMAIN: &[u8] = b"windshare/account-ticket/v1\0";
const AUDIENCE: &str = "official";

#[cfg(test)]
#[path = "ticket_tests.rs"]
mod tests;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct Ticket {
    pub issuer: Issuer,
    pub key_id: u64,
    pub account: AccountId,
    pub credential: CredentialId,
    pub issued_at: u64,
    pub expires_at: u64,
    pub entitlement: Entitlement,
}

#[expect(
    clippy::suspicious_operation_groupings,
    reason = "ticket key IDs and issue times bind certificate generation numbers and validity bounds"
)]
impl Ticket {
    pub const fn attribution(self) -> Attribution {
        Attribution::Account {
            issuer: self.issuer,
            account: self.account,
            credential: self.credential,
        }
    }

    pub fn encode(self) -> Vec<u8> {
        let mut out = Encoder::new();
        out.array(9);
        out.uint(1);
        out.bytes(&self.issuer);
        out.uint(self.key_id);
        out.bytes(&self.account);
        out.bytes(&self.credential);
        out.uint(self.issued_at);
        out.uint(self.expires_at);
        out.text(AUDIENCE);
        self.entitlement.encode(&mut out);
        out.0
    }

    fn decode(bytes: &[u8]) -> Result<Self, Error> {
        let mut input = Decoder::new(bytes);
        input.array(9)?;
        if input.uint()? != 1 {
            return Err(Error::Encoding);
        }
        let issuer = input.bytes()?;
        let key_id = input.uint()?;
        let account = input.bytes()?;
        let credential = input.bytes()?;
        let issued_at = input.uint()?;
        let expires_at = input.uint()?;
        input.text(AUDIENCE)?;
        let entitlement = Entitlement::decode(&mut input)?;
        input.end()?;
        let value = Self {
            issuer,
            key_id,
            account,
            credential,
            issued_at,
            expires_at,
            entitlement,
        };
        if issuer == [0; 32]
            || account == [0; 16]
            || credential == [0; 16]
            || expires_at <= issued_at
            || expires_at - issued_at > TICKET_LIFETIME
            || !entitlement.valid_snapshot(issued_at)
        {
            return Err(Error::Lifetime);
        }
        Ok(value)
    }

    /// # Errors
    /// Rejects a signing key or lifetime outside the certified generation.
    pub fn issue(self, generation: Generation, key: &SigningKey) -> Result<Vec<u8>, Error> {
        Self::decode(&self.encode())?;
        if self.issuer != generation.issuer
            || self.key_id != generation.number
            || self.issued_at < generation.not_before
            || self.expires_at > generation.expires_at
            || key.verifying_key().to_bytes() != generation.key
        {
            return Err(Error::Untrusted);
        }
        Ok(trust::sign(TICKET_DOMAIN, &self.encode(), key))
    }

    /// # Errors
    /// Rejects noncanonical, tampered, expired or untrusted tickets.
    pub fn verify(signed: &[u8], trusted: &TrustStore, now: u64) -> Result<Self, Error> {
        if signed.len() > MAX_TICKET_BYTES {
            return Err(Error::Encoding);
        }
        let length = signed.len().checked_sub(64).ok_or(Error::Encoding)?;
        let ticket = Self::decode(&signed[..length])?;
        let generation = trusted.current(ticket.issuer, ticket.key_id, now)?;
        trust::verify(TICKET_DOMAIN, signed, &generation.key)?;
        if ticket.issued_at < generation.not_before
            || ticket.expires_at > generation.expires_at
            || now.saturating_add(trust::CLOCK_SKEW) < ticket.issued_at
            || now >= ticket.expires_at
        {
            return Err(Error::Expired);
        }
        Ok(ticket)
    }
}
