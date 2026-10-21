//! `DESCRIPTOR_UPLOAD` (`WS2U`): the sealed descriptor a fresh registration
//! publishes, carried as opaque bytes the relay never parses.

use core::{error::Error, fmt};

use crate::{
    registration::FreshRegistration,
    wire::{FrameError, Magic, open_reserved},
};

pub(crate) const MAGIC: Magic = *b"WS2U";

/// Largest sealed descriptor a relay stores, shared with every client.
pub const MAX_DESCRIPTOR_BYTES: usize = 16 * 1024;

/// A decoded upload borrowing its object bytes from the received frame.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct DescriptorUpload<'a> {
    object: &'a [u8],
}

impl<'a> DescriptorUpload<'a> {
    /// Decodes one complete `WS2U` frame.
    ///
    /// # Errors
    ///
    /// Returns a [`FrameError`] unless the header is the v2 upload header and
    /// the declared length equals a nonempty object within
    /// [`MAX_DESCRIPTOR_BYTES`].
    pub fn decode(encoded: &'a [u8]) -> Result<Self, FrameError> {
        let mut body = open_reserved(encoded, MAGIC)?;
        let declared = body.u32()?;
        let object = body.remainder();
        let bounded = !object.is_empty() && object.len() <= MAX_DESCRIPTOR_BYTES;
        if !bounded || usize::try_from(declared) != Ok(object.len()) {
            return Err(FrameError::Length);
        }
        Ok(Self { object })
    }

    /// Returns the uploaded sealed-object bytes.
    #[must_use]
    pub const fn object(&self) -> &'a [u8] {
        self.object
    }
}

/// Descriptor bytes a fresh registration may publish.
///
/// This proves upload authority, not receiver usability: receivers still
/// authenticate the sealed object before trusting any of its contents.
#[derive(Debug, Eq, PartialEq)]
pub struct AuthorizedDescriptor<'a> {
    registration: FreshRegistration,
    object: &'a [u8],
}

impl<'a> AuthorizedDescriptor<'a> {
    pub(crate) const fn new(registration: FreshRegistration, object: &'a [u8]) -> Self {
        Self {
            registration,
            object,
        }
    }

    /// Returns the registration this descriptor was committed by.
    #[must_use]
    pub const fn registration(&self) -> &FreshRegistration {
        &self.registration
    }

    /// Returns the exact bytes to deliver to joining receivers.
    #[must_use]
    pub const fn object(&self) -> &'a [u8] {
        self.object
    }
}

/// The uploaded bytes are not the descriptor the registration committed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct DescriptorDigestMismatch;

impl fmt::Display for DescriptorDigestMismatch {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("uploaded descriptor does not match the registered digest")
    }
}

impl Error for DescriptorDigestMismatch {}
