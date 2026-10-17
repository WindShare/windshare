//! Cumulative WS2B wire feedback, staged for the coordinated client cutover.

use super::{
    Counts,
    totals::{DeliveryTotals, FRAME_BYTES},
};
use crate::{identifiers::RelaySessionId, wire::FrameError};

/// A complete consumed WS2O prefix, never initial capacity or a credit delta.
///
/// The authenticated connection supplies its generation and receive direction.
/// Mapping this frame to `share::Input::Consumed` must retain that exact lifetime;
/// decoding alone cannot authorize a refund or final settlement.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct DeliveryConsumed(DeliveryTotals);

impl DeliveryConsumed {
    pub(crate) const MAGIC: [u8; 4] = *b"WS2B";

    /// Validates envelope totals independently of runtime window sizes.
    ///
    /// # Errors
    ///
    /// Rejects counts that cannot describe complete WS2O envelopes.
    pub const fn new(session: RelaySessionId, total: Counts) -> Result<Self, FrameError> {
        match DeliveryTotals::new(session, total) {
            Ok(totals) => Ok(Self(totals)),
            Err(error) => Err(error),
        }
    }

    /// # Errors
    ///
    /// Rejects malformed framing, zero session IDs and impossible totals.
    pub fn decode(encoded: &[u8]) -> Result<Self, FrameError> {
        DeliveryTotals::decode(encoded, Self::MAGIC).map(Self)
    }

    #[must_use]
    pub fn encode(self) -> [u8; FRAME_BYTES] {
        self.0.encode(Self::MAGIC)
    }

    #[must_use]
    pub const fn session(self) -> RelaySessionId {
        self.0.session
    }

    #[must_use]
    pub const fn total(self) -> Counts {
        self.0.total
    }
}

#[cfg(test)]
#[path = "consumed_tests.rs"]
mod tests;
