use super::Counts;
use crate::{
    identifiers::RelaySessionId,
    wire::{FrameError, Magic, Writer, open_reserved},
};

pub(super) const FRAME_BYTES: usize = 8 + 8 + 8 + 8;

/// Shared layout only: each public control retains its own wire identity and meaning.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) struct DeliveryTotals {
    pub session: RelaySessionId,
    pub total: Counts,
}

impl DeliveryTotals {
    pub const fn new(session: RelaySessionId, total: Counts) -> Result<Self, FrameError> {
        if !total.is_envelope_batch() {
            return Err(FrameError::Field);
        }
        Ok(Self { session, total })
    }

    pub fn decode(encoded: &[u8], magic: Magic) -> Result<Self, FrameError> {
        let mut body = open_reserved(encoded, magic)?;
        let session = RelaySessionId::new(body.array()?).ok_or(FrameError::Field)?;
        let total = Counts::new(
            u64::from_be_bytes(body.array()?),
            u64::from_be_bytes(body.array()?),
        );
        body.finish()?;
        Self::new(session, total)
    }

    pub fn encode(self, magic: Magic) -> [u8; FRAME_BYTES] {
        let mut writer = Writer::with_header(magic, 0);
        writer
            .put(self.session.as_bytes())
            .put(&self.total.frames.to_be_bytes())
            .put(&self.total.encoded_bytes.to_be_bytes());
        writer.into_frame()
    }
}
