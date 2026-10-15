//! Sender-signature acceptance shared with Go and browser clients.

use core::{error::Error, fmt};

use ed25519_dalek::{PUBLIC_KEY_LENGTH, Signature, VerifyingKey};

/// An owned, canonical, nonidentity, prime-order Ed25519 public key.
///
/// Construction establishes key validity, not proof of possession or relay
/// authority. Callers must separately verify the operation's bound message.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SenderPublicKey {
    key: VerifyingKey,
}

impl SenderPublicKey {
    /// Checks the sender identity once so it can be retained across proofs.
    ///
    /// # Errors
    ///
    /// Returns `InvalidPublicKey` unless the input is exactly 32 bytes encoding
    /// a canonical, nonidentity point in the prime-order subgroup.
    pub fn from_bytes(public_key: &[u8]) -> Result<Self, InvalidPublicKey> {
        let bytes = public_key.try_into().map_err(|_| InvalidPublicKey)?;
        let key = VerifyingKey::from_bytes(bytes).map_err(|_| InvalidPublicKey)?;
        let point = key.to_edwards();

        // Dalek accepts ZIP-215 point encodings; verify_strict alone does not
        // reject every mixed-order key. Retained identities need both checks.
        if point.compress().as_bytes() != bytes
            || point.is_small_order()
            || !point.is_torsion_free()
        {
            return Err(InvalidPublicKey);
        }

        Ok(Self { key })
    }

    /// Returns an independent copy for identity binding without mutable access
    /// to the retained verification key.
    #[must_use]
    pub fn to_bytes(&self) -> [u8; PUBLIC_KEY_LENGTH] {
        self.key.to_bytes()
    }

    /// Verifies ordinary Ed25519 over the exact message bytes supplied.
    ///
    /// The caller owns domain separation and operation-specific authorization.
    ///
    /// # Errors
    ///
    /// Returns `InvalidSignature` for any signature outside the shared acceptance
    /// rules, including incorrect lengths and signatures for another message.
    pub fn verify(&self, message: &[u8], signature: &[u8]) -> Result<(), InvalidSignature> {
        let signature = Signature::try_from(signature).map_err(|_| InvalidSignature)?;

        // With canonical, prime-order A, strict verification enforces S < L,
        // non-small R and the cofactorless equation. Its encoded-R comparison
        // also rejects noncanonical R, and a valid equation implies prime-order R.
        self.key
            .verify_strict(message, &signature)
            .map_err(|_| InvalidSignature)
    }
}

/// The supplied bytes do not encode an accepted sender public key.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct InvalidPublicKey;

impl fmt::Display for InvalidPublicKey {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .write_str("sender public key must be 32 bytes, canonical, nonidentity and prime-order")
    }
}

impl Error for InvalidPublicKey {}

/// The signature does not authenticate the supplied message under the sender key.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct InvalidSignature;

impl fmt::Display for InvalidSignature {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("invalid sender signature")
    }
}

impl Error for InvalidSignature {}

#[cfg(test)]
#[path = "sender_auth_tests.rs"]
mod tests;
