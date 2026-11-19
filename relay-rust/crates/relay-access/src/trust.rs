pub use crate::cbor::Error;
use crate::{
    Issuer,
    cbor::{Decoder, Encoder},
};
use alloc::{collections::BTreeMap, vec::Vec};
use ed25519_dalek::{Signature, Signer, SigningKey, VerifyingKey};
use serde::{Deserialize, Serialize};

pub const MAX_CERTIFICATE_LIFETIME: u64 = 180 * 86_400;
pub const CLOCK_SKEW: u64 = 300;
const CERTIFICATE_DOMAIN: &[u8] = b"windshare/authority-certificate/v1\0";

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct Generation {
    pub issuer: Issuer,
    pub number: u64,
    pub key: [u8; 32],
    pub not_before: u64,
    pub expires_at: u64,
}

impl Generation {
    pub fn encode(self) -> Vec<u8> {
        let mut out = Encoder::new();
        out.array(6);
        out.uint(1);
        out.bytes(&self.issuer);
        out.uint(self.number);
        out.bytes(&self.key);
        out.uint(self.not_before);
        out.uint(self.expires_at);
        out.0
    }

    /// # Errors
    /// Rejects malformed CBOR or a certificate outside the bounded lifetime.
    pub fn decode(bytes: &[u8]) -> Result<Self, Error> {
        let mut input = Decoder::new(bytes);
        input.array(6)?;
        if input.uint()? != 1 {
            return Err(Error::Encoding);
        }
        let value = Self {
            issuer: input.bytes()?,
            number: input.uint()?,
            key: input.bytes()?,
            not_before: input.uint()?,
            expires_at: input.uint()?,
        };
        input.end()?;
        if value.number == 0
            || value.number > (1u64 << 53) - 1
            || value.issuer == [0; 32]
            || value.expires_at <= value.not_before
            || value.expires_at - value.not_before > MAX_CERTIFICATE_LIFETIME
        {
            return Err(Error::Lifetime);
        }
        Ok(value)
    }

    /// # Errors
    /// The supplied root must own this issuer namespace.
    pub fn certify(self, root: &SigningKey) -> Result<Vec<u8>, Error> {
        if root.verifying_key().to_bytes() != self.issuer {
            return Err(Error::Untrusted);
        }
        Self::decode(&self.encode())?;
        Ok(sign(CERTIFICATE_DOMAIN, &self.encode(), root))
    }

    /// # Errors
    /// The certificate must authenticate under the pinned root.
    pub fn verify(certificate: &[u8], root: Issuer) -> Result<Self, Error> {
        let payload = verify(CERTIFICATE_DOMAIN, certificate, &root)?;
        let generation = Self::decode(payload)?;
        if generation.issuer != root {
            return Err(Error::Untrusted);
        }
        Ok(generation)
    }

    pub const fn usable(self, now: u64) -> bool {
        now.saturating_add(CLOCK_SKEW) >= self.not_before && now < self.expires_at
    }
}

/// Hosts persist this high-water mark before activating a new certified generation.
/// An expired certificate never erases the rollback barrier.
#[derive(Clone, Debug, Default, Serialize, Deserialize)]
pub struct TrustStore {
    generations: BTreeMap<Issuer, Generation>,
}

impl TrustStore {
    pub fn generation(&self, issuer: Issuer) -> Option<Generation> {
        self.generations.get(&issuer).copied()
    }
    /// # Errors
    /// Refuses expired generations, rollback and same-generation key replacement.
    pub fn accept(&mut self, generation: Generation, now: u64) -> Result<(), Error> {
        if !generation.usable(now) {
            return Err(Error::Expired);
        }
        self.remember(generation)
    }

    /// # Errors
    /// Refuses malformed generations, rollback and equivocation, including after expiry.
    pub fn remember(&mut self, generation: Generation) -> Result<(), Error> {
        Generation::decode(&generation.encode())?;
        if let Some(previous) = self.generations.get(&generation.issuer)
            && (generation.number < previous.number
                || generation.number == previous.number && generation != *previous)
        {
            return Err(Error::Rollback);
        }
        self.generations.insert(generation.issuer, generation);
        Ok(())
    }

    /// # Errors
    /// Only the current, unexpired signing generation can authenticate new admissions.
    pub fn current(&self, issuer: Issuer, number: u64, now: u64) -> Result<Generation, Error> {
        let generation = self.generation(issuer).ok_or(Error::Untrusted)?;
        if generation.number != number {
            return Err(Error::Rollback);
        }
        if !generation.usable(now) {
            return Err(Error::Expired);
        }
        Ok(generation)
    }
}

pub fn sign(domain: &[u8], payload: &[u8], key: &SigningKey) -> Vec<u8> {
    let mut preimage = Vec::with_capacity(domain.len() + payload.len());
    preimage.extend_from_slice(domain);
    preimage.extend_from_slice(payload);
    let mut result = payload.to_vec();
    result.extend_from_slice(&key.sign(&preimage).to_bytes());
    result
}

/// # Errors
/// Refuses truncated records, invalid keys and invalid Ed25519 signatures.
pub fn verify<'a>(domain: &[u8], signed: &'a [u8], key: &[u8; 32]) -> Result<&'a [u8], Error> {
    let length = signed.len().checked_sub(64).ok_or(Error::Encoding)?;
    let (payload, signature) = signed.split_at(length);
    let signature = Signature::from_slice(signature).map_err(|_| Error::Signature)?;
    let key = VerifyingKey::from_bytes(key).map_err(|_| Error::Signature)?;
    let mut preimage = Vec::with_capacity(domain.len() + payload.len());
    preimage.extend_from_slice(domain);
    preimage.extend_from_slice(payload);
    key.verify_strict(&preimage, &signature)
        .map_err(|_| Error::Signature)?;
    Ok(payload)
}
