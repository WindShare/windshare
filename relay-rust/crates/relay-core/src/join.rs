//! `JOIN` (`WS2J`): a receiver asks to reach the sender of one share.
//!
//! Joining carries no sender authority: the share capability is end to end,
//! so the relay only learns which route the receiver wants.

use core::time::Duration;

use crate::{
    clock::UnixMillis,
    identifiers::{RelayIdentity, ShareId},
    lifecycle::ConnectionRef,
    wire::{FrameError, Magic, open_reserved},
};

pub(crate) const JOIN_MAGIC: Magic = *b"WS2J";

// A provisional session must complete E2E admission promptly; browsing and P2P
// silence after admission do not consume this budget.
pub(crate) const SESSION_ADMISSION_TIMEOUT: Duration = Duration::from_secs(30);

/// A decoded JOIN bound to its connection and original admission deadline.
///
/// Only the connection handshake creates this move-only request, so delayed
/// host admission cannot reset the budget or silently change its participant.
#[derive(Debug, Eq, PartialEq)]
pub struct JoinRequest {
    relay: RelayIdentity,
    connection: ConnectionRef,
    share: ShareId,
    deadline: UnixMillis,
}

impl JoinRequest {
    pub(crate) fn new(
        relay: RelayIdentity,
        connection: ConnectionRef,
        share: ShareId,
        now: UnixMillis,
    ) -> Self {
        Self {
            relay,
            connection,
            share,
            deadline: now.saturating_add(SESSION_ADMISSION_TIMEOUT),
        }
    }

    /// Relay of the receiving handshake.
    #[must_use]
    pub const fn relay(&self) -> RelayIdentity {
        self.relay
    }

    /// Exact receiver lifetime.
    #[must_use]
    pub const fn connection(&self) -> ConnectionRef {
        self.connection
    }

    /// Requested share.
    #[must_use]
    pub const fn share(&self) -> ShareId {
        self.share
    }

    /// Absolute budget from receiving JOIN, including host preparation.
    #[must_use]
    pub const fn deadline(&self) -> UnixMillis {
        self.deadline
    }
}

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
