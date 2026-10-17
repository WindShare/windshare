//! Cumulative WS2B wire feedback, staged for the coordinated client cutover.

use super::Counts;
use crate::{
    identifiers::RelaySessionId,
    wire::{FrameError, Writer, open_reserved},
};

const MAGIC: [u8; 4] = *b"WS2B";
const FRAME_BYTES: usize = 8 + 8 + 8 + 8;

/// A complete consumed WS2O prefix, never initial capacity or a credit delta.
///
/// The authenticated connection supplies its generation and receive direction.
/// Mapping this frame to `share::Input::Consumed` must retain that exact lifetime;
/// decoding alone cannot authorize a refund or final settlement.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct DeliveryConsumed {
    session: RelaySessionId,
    total: Counts,
}

impl DeliveryConsumed {
    /// Validates envelope totals independently of runtime window sizes.
    ///
    /// # Errors
    ///
    /// Rejects counts that cannot describe complete WS2O envelopes.
    pub const fn new(session: RelaySessionId, total: Counts) -> Result<Self, FrameError> {
        if !total.is_envelope_batch() {
            return Err(FrameError::Field);
        }
        Ok(Self { session, total })
    }

    /// # Errors
    ///
    /// Rejects malformed framing, zero session IDs and impossible totals.
    pub fn decode(encoded: &[u8]) -> Result<Self, FrameError> {
        let mut body = open_reserved(encoded, MAGIC)?;
        let session = RelaySessionId::new(body.array()?).ok_or(FrameError::Field)?;
        let total = Counts::new(
            u64::from_be_bytes(body.array()?),
            u64::from_be_bytes(body.array()?),
        );
        body.finish()?;
        Self::new(session, total)
    }

    #[must_use]
    pub fn encode(self) -> [u8; FRAME_BYTES] {
        let mut writer = Writer::with_header(MAGIC, 0);
        writer
            .put(self.session.as_bytes())
            .put(&self.total.frames.to_be_bytes())
            .put(&self.total.encoded_bytes.to_be_bytes());
        writer.into_frame()
    }

    #[must_use]
    pub const fn session(self) -> RelaySessionId {
        self.session
    }

    #[must_use]
    pub const fn total(self) -> Counts {
        self.total
    }
}

#[cfg(test)]
#[path = "consumed_tests.rs"]
mod tests;
