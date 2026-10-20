//! `RELAY_ERROR` (`WS2E`): the relay's refusal of a control request.

use crate::wire::{Magic, WIRE_VERSION, Writer};

const MAGIC: Magic = *b"WS2E";

/// Delay hint for a refusal that a client may retry.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct RetryAfter(u32);

impl RetryAfter {
    /// Longest delay a client honours; longer waits must be refused outright.
    pub const MAX_MILLISECONDS: u32 = 30_000;

    /// Asks the client to retry immediately.
    pub const IMMEDIATE: Self = Self(0);

    /// Returns `None` beyond [`Self::MAX_MILLISECONDS`].
    #[must_use]
    pub const fn from_millis(milliseconds: u32) -> Option<Self> {
        if milliseconds > Self::MAX_MILLISECONDS {
            return None;
        }
        Some(Self(milliseconds))
    }
}

/// A refusal on the wire.
///
/// Only transient refusals carry a [`RetryAfter`], so a permanent refusal can
/// never ask a client to come back.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub enum ErrorFrame {
    /// The request violated the frame layout or flow.
    Malformed,
    /// The registration mode is not supported.
    UnsupportedMode,
    /// Another sender key already owns the share ID.
    ShareIdCollision,
    /// The share is already registered.
    AlreadyRegistered,
    /// The challenge expired before the proof arrived.
    ChallengeExpired,
    /// The sender proof or resume credential was not accepted.
    InvalidProof,
    /// The uploaded descriptor was not accepted.
    DescriptorInvalid,
    /// No route exists for the share.
    NotFound,
    /// The route is being established; retry after the hint.
    Starting(RetryAfter),
    /// Relay capacity refused admission; retry after the hint.
    Admission(RetryAfter),
    /// The share instance was explicitly stopped and never resumes.
    Stopped,
    /// The resume refers to a superseded registration.
    ResumeStale,
    /// Service credentials need user action; retries cannot repair them.
    AccessDenied,
    /// Access authority is temporarily unavailable; share progress is retained.
    AccessUnavailable(RetryAfter),
}

impl ErrorFrame {
    /// Encoded `WS2E` width in bytes.
    pub const ENCODED_BYTES: usize = 12;

    const fn code(self) -> u16 {
        match self {
            Self::Malformed => 1,
            Self::UnsupportedMode => 2,
            Self::ShareIdCollision => 3,
            Self::AlreadyRegistered => 4,
            Self::ChallengeExpired => 5,
            Self::InvalidProof => 6,
            Self::DescriptorInvalid => 7,
            Self::NotFound => 8,
            Self::Starting(_) => 9,
            Self::Admission(_) => 10,
            Self::Stopped => 11,
            Self::ResumeStale => 12,
            Self::AccessDenied => 13,
            Self::AccessUnavailable(_) => 14,
        }
    }

    const fn retry_after(self) -> RetryAfter {
        match self {
            Self::Starting(retry) | Self::Admission(retry) | Self::AccessUnavailable(retry) => {
                retry
            }
            _ => RetryAfter::IMMEDIATE,
        }
    }

    /// Encodes the refusal sent to the client.
    #[must_use]
    pub fn encode(self) -> [u8; Self::ENCODED_BYTES] {
        // WS2E has a six-byte header: its code occupies the two bytes other
        // control frames reserve, leaving only byte 5 reserved.
        let mut frame = Writer::new();
        frame
            .put(&MAGIC)
            .put(&[WIRE_VERSION, 0])
            .put(&self.code().to_be_bytes())
            .put(&self.retry_after().0.to_be_bytes());
        frame.into_frame()
    }
}

#[cfg(test)]
#[path = "error_frame_tests.rs"]
mod tests;
