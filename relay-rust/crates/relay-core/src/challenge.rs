//! CHALLENGE (`WS2Q`): relay-issued freshness that every sender proof signs.

use crate::{
    clock::{UnixMillis, UnixSeconds},
    identifiers::{ChallengeId, ChallengeNonce},
    wire::{Magic, Writer},
};

const MAGIC: Magic = *b"WS2Q";

/// The single operation a challenge may authenticate.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub enum ChallengePurpose {
    /// A fresh `REGISTER`.
    Register,
    /// A `RESUME` of an existing registration.
    Resume,
    /// A `STOP` of a share instance.
    Stop,
}

impl ChallengePurpose {
    const fn wire(self) -> u8 {
        match self {
            Self::Register => 0,
            Self::Resume => 1,
            Self::Stop => 2,
        }
    }
}

/// Unpredictable values a host draws to issue one challenge.
///
/// Move-only like the challenge it seeds: reusing a seed would issue two
/// challenges that one signature could answer.
#[derive(Debug)]
pub struct ChallengeSeed {
    id: ChallengeId,
    nonce: ChallengeNonce,
}

impl ChallengeSeed {
    pub(crate) fn checkpoint(&self) -> [u8; 48] {
        let mut bytes = [0; 48];
        bytes[..16].copy_from_slice(self.id.as_bytes());
        bytes[16..].copy_from_slice(self.nonce.as_bytes());
        bytes
    }

    /// Returns `None` when either value is all zeros, which the wire reserves
    /// and only a broken random source produces.
    #[must_use]
    pub fn new(id: [u8; 16], nonce: [u8; 32]) -> Option<Self> {
        Some(Self {
            id: ChallengeId::new(id)?,
            nonce: ChallengeNonce::new(nonce)?,
        })
    }
}

/// An issued challenge.
///
/// It is deliberately neither `Copy` nor `Clone`: authentication consumes it,
/// so the type system enforces the one-use rule a replayed proof would break.
#[derive(Debug, Eq, PartialEq)]
pub struct Challenge {
    purpose: ChallengePurpose,
    id: ChallengeId,
    nonce: ChallengeNonce,
    expires_at: UnixSeconds,
}

impl Challenge {
    /// Encoded `WS2Q` width in bytes.
    pub const ENCODED_BYTES: usize = 64;

    /// Returns `None` for a zero expiry, which no client accepts.
    #[must_use]
    pub const fn new(
        purpose: ChallengePurpose,
        id: ChallengeId,
        nonce: ChallengeNonce,
        expires_at: UnixSeconds,
    ) -> Option<Self> {
        if expires_at.0 == 0 {
            return None;
        }
        Some(Self {
            purpose,
            id,
            nonce,
            expires_at,
        })
    }

    /// Issues a challenge that expires with the handshake holding it.
    ///
    /// A challenge never outlives its connection's handshake, so its expiry is
    /// that handshake's deadline rounded up to whole wire seconds instead of an
    /// independent lifetime. Handshake deadlines lie at least one timeout past
    /// the epoch, so the rounded expiry is never the zero the wire reserves.
    #[expect(
        clippy::needless_pass_by_value,
        reason = "consuming the seed enforces its one-use rule"
    )]
    pub(crate) const fn issue(
        purpose: ChallengePurpose,
        seed: ChallengeSeed,
        deadline: UnixMillis,
    ) -> Self {
        Self {
            purpose,
            id: seed.id,
            nonce: seed.nonce,
            expires_at: deadline.ceil_seconds(),
        }
    }

    /// Returns the operation this challenge may authenticate.
    #[must_use]
    pub const fn purpose(&self) -> ChallengePurpose {
        self.purpose
    }

    /// Returns the first second at which proofs are no longer accepted.
    #[must_use]
    pub const fn expires_at(&self) -> UnixSeconds {
        self.expires_at
    }

    /// A proof is accepted strictly before the expiry second, matching the
    /// half-open interval every client signs against.
    #[must_use]
    pub const fn is_live_at(&self, now: UnixMillis) -> bool {
        now.0 < self.expires_at.start().0
    }

    /// Encodes the challenge sent to the client.
    #[must_use]
    pub fn encode(&self) -> [u8; Self::ENCODED_BYTES] {
        let mut frame = Writer::with_header(MAGIC, self.purpose.wire());
        self.put_body(&mut frame);
        frame.into_frame()
    }

    /// Appends the challenge body, which every operation preimage also signs
    /// so a proof cannot outlive or migrate away from the challenge it answers.
    pub(crate) fn put_body<const N: usize>(&self, destination: &mut Writer<N>) {
        destination
            .put(self.id.as_bytes())
            .put(self.nonce.as_bytes())
            .put(&self.expires_at.0.to_be_bytes());
    }
}
