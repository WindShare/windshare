//! CHALLENGE (`WS2Q`): relay-issued freshness that every sender proof signs.

use crate::{
    identifiers::{ChallengeId, ChallengeNonce},
    wire::{Magic, Writer},
};

const MAGIC: Magic = *b"WS2Q";

/// Seconds since the Unix epoch, as read from the host clock.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct UnixSeconds(pub u64);

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
    pub const fn is_live_at(&self, now: UnixSeconds) -> bool {
        now.0 < self.expires_at.0
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
