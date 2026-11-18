//! Advisory congestion is separate from routing authority and socket liveness.
use super::Direction;
#[cfg(test)]
#[path = "capacity_tests.rs"]
mod tests;

use crate::{FrameError, identifiers::RelaySessionId, wire::Writer};

/// A short, renewable observation of a pending flow. Zero clears that direction.
/// It never extends an admission, credential, entitlement or heartbeat deadline.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CapacityWait {
    pub session: RelaySessionId,
    pub direction: Direction,
    pub milliseconds: u32,
}

impl CapacityWait {
    pub const ENCODED_BYTES: usize = 8 + 8 + 4;
    pub const MAX_MILLISECONDS: u32 = 60_000;

    /// # Errors
    /// Rejects a lease too long to preserve bounded recovery.
    pub fn encode(self) -> Result<[u8; Self::ENCODED_BYTES], FrameError> {
        if self.milliseconds > Self::MAX_MILLISECONDS {
            return Err(FrameError::Field);
        }
        let direction = match self.direction {
            Direction::ToSender => 0,
            Direction::ToReceiver => 1,
        };
        let mut out = Writer::with_header(*b"WS2Z", direction);
        out.put(self.session.as_bytes())
            .put(&self.milliseconds.to_be_bytes());
        Ok(out.into_frame())
    }
}
