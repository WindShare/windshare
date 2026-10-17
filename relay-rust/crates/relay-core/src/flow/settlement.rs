//! Ordered retirement controls, distinct from ordinary application consumption.

use super::{
    Counts, Settlement,
    totals::{DeliveryTotals, FRAME_BYTES},
};
use crate::{identifiers::RelaySessionId, wire::FrameError};

/// Relay-to-destination notice following every submitted WS2O for this flow.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SettlementNotice(DeliveryTotals);

impl SettlementNotice {
    const MAGIC: [u8; 4] = *b"WS2N";

    /// # Errors
    /// Rejects totals that cannot describe complete WS2O envelopes.
    pub const fn new(session: RelaySessionId, total: Counts) -> Result<Self, FrameError> {
        match DeliveryTotals::new(session, total) {
            Ok(totals) => Ok(Self(totals)),
            Err(error) => Err(error),
        }
    }

    /// # Errors
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

impl TryFrom<Settlement> for SettlementNotice {
    type Error = FrameError;

    fn try_from(notice: Settlement) -> Result<Self, FrameError> {
        Self::new(notice.flow.session().wire_id(), notice.submitted)
    }
}

/// Destination-to-relay acknowledgement after consuming or discarding the notice's prefix.
/// The authenticated socket selects the direction and exact connection generation.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SettlementAck(DeliveryTotals);

impl SettlementAck {
    pub(crate) const MAGIC: [u8; 4] = *b"WS2S";

    /// # Errors
    /// Rejects totals that cannot describe complete WS2O envelopes.
    pub const fn new(session: RelaySessionId, total: Counts) -> Result<Self, FrameError> {
        match DeliveryTotals::new(session, total) {
            Ok(totals) => Ok(Self(totals)),
            Err(error) => Err(error),
        }
    }

    /// # Errors
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
#[path = "settlement_tests.rs"]
mod tests;
