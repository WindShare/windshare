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

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum VerificationError {
    Invalid(Error),
    /// The presented key ID is unverified and can only inform a refusal.
    GenerationMismatch {
        issuer: Issuer,
        presented_generation: u64,
        installed_generation: u64,
    },
}

impl From<Error> for VerificationError {
    fn from(error: Error) -> Self {
        Self::Invalid(error)
    }
}

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
            || !Generation::valid_number(key_id)
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
    /// Only the current, usable generation can authenticate a ticket.
    pub fn verify(
        signed: &[u8],
        trusted: &TrustStore,
        now: u64,
    ) -> Result<Self, VerificationError> {
        VerifiedTicket::verify_with(signed, now, |issuer| trusted.generation(issuer))
            .map(|verified| verified.ticket)
    }

    fn matching_generation(
        self,
        installed: Option<Generation>,
        now: u64,
    ) -> Result<Generation, VerificationError> {
        if now.saturating_add(trust::CLOCK_SKEW) < self.issued_at || now >= self.expires_at {
            return Err(Error::Expired.into());
        }
        let generation = installed
            .filter(|generation| generation.issuer == self.issuer)
            .ok_or(Error::Untrusted)?;
        if generation.number != self.key_id {
            return Err(VerificationError::GenerationMismatch {
                issuer: self.issuer,
                presented_generation: self.key_id,
                installed_generation: generation.number,
            });
        }
        if !generation.usable(now)
            || self.issued_at < generation.not_before
            || self.expires_at > generation.expires_at
        {
            return Err(Error::Expired.into());
        }
        Ok(generation)
    }
}

/// Signature evidence tied to the exact certified generation. A later admission
/// must still consult the live trust store and node policy.
pub struct VerifiedTicket {
    ticket: Ticket,
    generation: Generation,
}

impl VerifiedTicket {
    /// Takes a snapshot of the issuer's current generation before verifying the signature.
    ///
    /// # Errors
    /// Rejects an invalid ticket and reports a mismatched generation separately.
    pub fn verify_with(
        signed: &[u8],
        now: u64,
        installed_generation: impl FnOnce(Issuer) -> Option<Generation>,
    ) -> Result<Self, VerificationError> {
        if signed.len() > MAX_TICKET_BYTES {
            return Err(Error::Encoding.into());
        }
        let length = signed.len().checked_sub(64).ok_or(Error::Encoding)?;
        let ticket = Ticket::decode(&signed[..length])?;
        let generation = ticket.matching_generation(installed_generation(ticket.issuer), now)?;
        trust::verify(TICKET_DOMAIN, signed, &generation.key)?;
        Ok(Self { ticket, generation })
    }

    /// # Errors
    /// Rejects evidence whose ticket or signing generation is no longer current.
    pub fn current(&self, trusted: &TrustStore, now: u64) -> Result<Ticket, VerificationError> {
        let ticket = self.ticket;
        let generation = ticket.matching_generation(trusted.generation(ticket.issuer), now)?;
        if self.generation != generation {
            return Err(Error::Untrusted.into());
        }
        Ok(ticket)
    }
}
