//! Fixed-width identities that relay control frames carry and proofs bind.

use core::fmt;

use sha2::{Digest, Sha256};

use crate::{sender_auth::SenderPublicKey, wire::FrameError};

const SENDER_KEY_DOMAIN: &[u8] = b"windshare/v2 sender-key\0";
const SHARE_ID_DOMAIN: &[u8] = b"windshare/v2 share-id\0";

macro_rules! nonzero_identifier {
    ($(#[$meta:meta])* $name:ident, $width:literal) => {
        $(#[$meta])*
        #[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
        pub struct $name([u8; $width]);

        impl $name {
            /// Accepts any value except all zeros, which the wire contract
            /// reserves for an absent identity and never authenticates.
            #[must_use]
            pub fn new(bytes: [u8; $width]) -> Option<Self> {
                (bytes != [0; $width]).then_some(Self(bytes))
            }

            /// Returns the encoded identity.
            #[must_use]
            pub const fn as_bytes(&self) -> &[u8; $width] {
                &self.0
            }
        }
    };
}

macro_rules! digest_identifier {
    ($(#[$meta:meta])* $name:ident, $width:literal) => {
        $(#[$meta])*
        #[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
        pub struct $name([u8; $width]);

        impl $name {
            /// Accepts every value: digests have no reserved encoding.
            #[must_use]
            pub const fn new(bytes: [u8; $width]) -> Self {
                Self(bytes)
            }

            /// Returns the encoded digest.
            #[must_use]
            pub const fn as_bytes(&self) -> &[u8; $width] {
                &self.0
            }
        }
    };
}

nonzero_identifier!(
    /// Routing identity of a share; valid only as the image of its key hash.
    ShareId,
    12
);
nonzero_identifier!(
    /// One content instance of a share, stable across relay reconnections.
    ShareInstance,
    16
);
nonzero_identifier!(
    /// Digest of the canonical relay endpoint that sender proofs name.
    RelayIdentity,
    32
);
nonzero_identifier!(
    /// Relay-chosen identifier of one issued challenge.
    ChallengeId,
    16
);
nonzero_identifier!(
    /// Relay-chosen freshness that makes each sender proof one-use.
    ChallengeNonce,
    32
);
nonzero_identifier!(
    /// Sender-chosen identifier acknowledged by STOPPED.
    StopId,
    16
);

nonzero_identifier!(
    /// Host-allocated opaque session route, never reused on surviving connections.
    RelaySessionId,
    8
);

digest_identifier!(
    /// Domain-separated SHA-256 prefix of the share's sender public key.
    PkHash,
    16
);
digest_identifier!(
    /// SHA-256 of the exact sealed descriptor bytes a registration publishes.
    DescriptorDigest,
    32
);
digest_identifier!(
    /// SHA-256 of the sender-held resume token.
    ResumeTokenHash,
    32
);

impl PkHash {
    /// Derives the key commitment that share links and route identities carry.
    #[must_use]
    pub fn of(sender: &SenderPublicKey) -> Self {
        Self(sha256_prefix(&[SENDER_KEY_DOMAIN, &sender.to_bytes()]))
    }
}

impl DescriptorDigest {
    /// Commits to exact descriptor bytes without interpreting the sealed object.
    #[must_use]
    pub fn of(descriptor: &[u8]) -> Self {
        Self(sha256_prefix(&[descriptor]))
    }
}

/// A share ID proven to be the domain-separated image of a sender key hash.
///
/// Only matching pairs are representable, so a frame can never route one
/// sender's proof under another share's identity.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct ShareRoute {
    share_id: ShareId,
    pk_hash: PkHash,
}

impl ShareRoute {
    /// Pairs `share_id` with `pk_hash` only when the ID derives from the hash.
    #[must_use]
    pub fn new(share_id: ShareId, pk_hash: PkHash) -> Option<Self> {
        let derived: [u8; 12] = sha256_prefix(&[SHARE_ID_DOMAIN, pk_hash.as_bytes()]);
        (derived == share_id.0).then_some(Self { share_id, pk_hash })
    }

    pub(crate) fn from_wire(share_id: [u8; 12], pk_hash: [u8; 16]) -> Result<Self, FrameError> {
        ShareId::new(share_id)
            .and_then(|share_id| Self::new(share_id, PkHash(pk_hash)))
            .ok_or(FrameError::Field)
    }

    /// Returns the routing identity receivers join with.
    #[must_use]
    pub const fn share_id(&self) -> ShareId {
        self.share_id
    }

    /// Returns the key commitment every sender proof must match.
    #[must_use]
    pub const fn pk_hash(&self) -> PkHash {
        self.pk_hash
    }
}

/// Sender-held secret whose SHA-256 a fresh registration commits for resume.
///
/// The token is a credential, so it exposes only its commitment and never
/// prints its bytes in diagnostics.
#[derive(Clone, Copy)]
pub struct ResumeToken([u8; 32]);

impl ResumeToken {
    pub(crate) fn new(bytes: [u8; 32]) -> Option<Self> {
        (bytes != [0; 32]).then_some(Self(bytes))
    }

    /// Returns the commitment a resume registration must already carry.
    #[must_use]
    pub fn hash(&self) -> ResumeTokenHash {
        ResumeTokenHash(sha256_prefix(&[&self.0]))
    }
}

impl fmt::Debug for ResumeToken {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("ResumeToken(<redacted>)")
    }
}

fn sha256_prefix<const N: usize>(parts: &[&[u8]]) -> [u8; N] {
    const { assert!(N <= 32, "SHA-256 output has 32 bytes") };
    let mut hasher = Sha256::new();
    for part in parts {
        hasher.update(part);
    }
    let digest = hasher.finalize();
    let mut prefix = [0; N];
    prefix.copy_from_slice(&digest[..N]);
    prefix
}
