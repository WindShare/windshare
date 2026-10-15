//! `JOIN` (`WS2J`): a receiver asks to reach the sender of one share.
//!
//! Joining carries no sender authority: the share capability is end to end,
//! so the relay only learns which route the receiver wants.

use crate::{
    identifiers::ShareId,
    wire::{FrameError, Magic, open_reserved},
};

pub(crate) const JOIN_MAGIC: Magic = *b"WS2J";

/// `JOIN` (`WS2J`): the share a receiver connection asks to join.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Join {
    share_id: ShareId,
}

impl Join {
    /// Decodes one complete `WS2J` frame.
    ///
    /// # Errors
    ///
    /// Returns a [`FrameError`] unless the frame has the exact v2 layout and a
    /// nonzero share ID.
    pub fn decode(encoded: &[u8]) -> Result<Self, FrameError> {
        let mut body = open_reserved(encoded, JOIN_MAGIC)?;
        let share_id = body.array()?;
        body.finish()?;
        Ok(Self {
            share_id: ShareId::new(share_id).ok_or(FrameError::Field)?,
        })
    }

    /// Returns the share the receiver asks to join.
    #[must_use]
    pub const fn share_id(&self) -> ShareId {
        self.share_id
    }
}
