//! Sender proof bodies and the reasons a relay refuses their authority.

use core::{error::Error, fmt};

use ed25519_dalek::{PUBLIC_KEY_LENGTH, SIGNATURE_LENGTH};

use crate::{
    identifiers::PkHash,
    sender_auth::SenderPublicKey,
    wire::{FrameError, Reader},
};

/// The key and signature body shared by `REGISTER_PROOF` and `STOP_PROOF`.
///
/// Decoding admits only accepted sender keys, so authentication never has to
/// handle a key that could not have produced a valid signature.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SenderProof {
    sender: SenderPublicKey,
    signature: [u8; SIGNATURE_LENGTH],
}

impl SenderProof {
    pub(crate) fn read(body: &mut Reader<'_>) -> Result<Self, FrameError> {
        let sender = SenderPublicKey::from_bytes(&body.array::<PUBLIC_KEY_LENGTH>()?)
            .map_err(|_| FrameError::Field)?;
        let signature = body.array::<SIGNATURE_LENGTH>()?;
        // Go and browser decoders reject a zero signature as an absent field;
        // rejecting it here keeps the three decoders' admission sets equal.
        if signature == [0; SIGNATURE_LENGTH] {
            return Err(FrameError::Field);
        }
        Ok(Self { sender, signature })
    }

    /// Returns the key the sender claims to hold.
    #[must_use]
    pub const fn sender(&self) -> &SenderPublicKey {
        &self.sender
    }

    /// Binds the claimed key to the share before verifying the exact preimage,
    /// so a valid signature from any other key never grants share authority.
    pub(crate) fn authenticate(
        &self,
        pk_hash: PkHash,
        preimage: &[u8],
    ) -> Result<(), AuthenticationError> {
        if PkHash::of(&self.sender) != pk_hash {
            return Err(AuthenticationError::SenderKeyMismatch);
        }
        self.sender
            .verify(preimage, &self.signature)
            .map_err(|_| AuthenticationError::InvalidSignature)
    }
}

/// Why a structurally valid sender proof does not grant relay authority.
///
/// Hosts close the connection without revealing which check failed, so the
/// variants serve traces and never become a key- or challenge-validity oracle.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AuthenticationError {
    /// The challenge was issued for a different operation.
    PurposeMismatch,
    /// The proof claims a different registration mode than its init frame.
    ModeMismatch,
    /// The proof arrived at or after the challenge expiry second.
    ChallengeExpired,
    /// The operation names a different relay identity.
    RelayMismatch,
    /// The proof key does not hash to the share's key commitment.
    SenderKeyMismatch,
    /// The signature does not authenticate the exact operation preimage.
    InvalidSignature,
}

impl fmt::Display for AuthenticationError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::PurposeMismatch => "challenge was issued for another operation",
            Self::ModeMismatch => "proof mode differs from the registration mode",
            Self::ChallengeExpired => "challenge expired before the proof arrived",
            Self::RelayMismatch => "operation names another relay",
            Self::SenderKeyMismatch => "proof key is not the share's sender key",
            Self::InvalidSignature => "signature does not authenticate the operation",
        })
    }
}

impl Error for AuthenticationError {}
