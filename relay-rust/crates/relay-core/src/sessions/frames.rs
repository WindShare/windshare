//! Fixed session controls and opaque envelope validation, without payload copies.

use crate::{
    descriptor_upload::MAX_DESCRIPTOR_BYTES,
    identifiers::RelaySessionId,
    wire::{FrameError, Writer, open_reserved},
};
use alloc::vec::Vec;

const OPAQUE_MAGIC: [u8; 4] = *b"WS2O";
const OPAQUE_HEADER_BYTES: u32 = 8 + 8 + 4;

pub const DESCRIPTOR_HEADER_BYTES: usize = 8 + 8 + 4;

/// Encode the sealed descriptor without inspecting its end-to-end contents.
///
/// # Errors
/// Rejects empty or oversized descriptors before allocating an outgoing frame.
pub fn encode_descriptor(session: RelaySessionId, object: &[u8]) -> Result<Vec<u8>, FrameError> {
    if object.is_empty() || object.len() > MAX_DESCRIPTOR_BYTES {
        return Err(FrameError::Length);
    }
    let length = u32::try_from(object.len()).map_err(|_| FrameError::Length)?;
    let mut header = Writer::<DESCRIPTOR_HEADER_BYTES>::with_header(*b"WS2D", 0);
    header.put(session.as_bytes()).put(&length.to_be_bytes());
    let mut encoded = Vec::with_capacity(DESCRIPTOR_HEADER_BYTES + object.len());
    encoded.extend_from_slice(&header.into_frame());
    encoded.extend_from_slice(object);
    Ok(encoded)
}

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
    /// A complete envelope includes its routing header and nonempty ciphertext.
    pub const MIN_ENCODED_BYTES: u32 = OPAQUE_HEADER_BYTES + 1;
    /// Maximum charge of one envelope to a delivery budget.
    pub const MAX_ENCODED_BYTES: u32 = OPAQUE_HEADER_BYTES + MAX_OPAQUE_CIPHERTEXT_BYTES;

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
    pub const MAGIC: [u8; 4] = *b"WS2M";

    /// Decodes the exact reserved-prefix and nonzero session-ID layout.
    ///
    /// # Errors
    ///
    /// Returns a `FrameError` for malformed, extended or truncated controls.
    pub fn decode(encoded: &[u8]) -> Result<Self, FrameError> {
        let mut body = open_reserved(encoded, Self::MAGIC)?;
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
