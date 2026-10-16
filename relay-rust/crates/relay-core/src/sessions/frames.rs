//! Fixed session controls and opaque envelope validation, without payload copies.

use crate::{
    identifiers::RelaySessionId,
    wire::{FrameError, open_reserved},
};

const OPAQUE_MAGIC: [u8; 4] = *b"WS2O";
const ADMITTED_MAGIC: [u8; 4] = *b"WS2M";

/// Maximum ciphertext in one relay envelope, shared by Go and browser codecs.
pub const MAX_OPAQUE_CIPHERTEXT_BYTES: u32 = 65_536;

/// Validated fixed metadata of a complete WS2O envelope.
///
/// Decoding borrows the host's frame only for length checks; neither payload
/// bytes nor their lifetime escape into the control engine or diagnostics.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct OpaqueHeader {
    session: RelaySessionId,
    encoded_bytes: usize,
}

impl OpaqueHeader {
    /// Validates version, reserved bytes, identity, bounds and exact frame length.
    ///
    /// # Errors
    ///
    /// Returns a `FrameError` for malformed or oversized envelopes.
    pub fn decode(encoded: &[u8]) -> Result<Self, FrameError> {
        let mut body = open_reserved(encoded, OPAQUE_MAGIC)?;
        let session = RelaySessionId::new(body.array()?).ok_or(FrameError::Field)?;
        let length = body.u32()?;
        if length == 0 || length > MAX_OPAQUE_CIPHERTEXT_BYTES {
            return Err(FrameError::Field);
        }
        if body.remainder().len() != length as usize {
            return Err(FrameError::Length);
        }
        Ok(Self {
            session,
            encoded_bytes: encoded.len(),
        })
    }

    /// Wire session ID for lookup and permit binding.
    #[must_use]
    pub const fn session(self) -> RelaySessionId {
        self.session
    }

    /// Complete encoded size, including the routing header, for host credit accounting.
    #[must_use]
    pub const fn encoded_bytes(self) -> usize {
        self.encoded_bytes
    }
}

/// WS2M: the sender retained a provisional relay session.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SessionAdmitted {
    session: RelaySessionId,
}

impl SessionAdmitted {
    /// Decodes the exact reserved-prefix and nonzero session-ID layout.
    ///
    /// # Errors
    ///
    /// Returns a `FrameError` for malformed, extended or truncated controls.
    pub fn decode(encoded: &[u8]) -> Result<Self, FrameError> {
        let mut body = open_reserved(encoded, ADMITTED_MAGIC)?;
        let session = RelaySessionId::new(body.array()?).ok_or(FrameError::Field)?;
        body.finish()?;
        Ok(Self { session })
    }

    /// Session the sender retained.
    #[must_use]
    pub const fn session(self) -> RelaySessionId {
        self.session
    }
}

#[cfg(test)]
#[path = "frames_tests.rs"]
mod tests;
