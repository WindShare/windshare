//! Exact-layout framing shared by relay control frames.
//!
//! Every control frame opens with a four-byte magic, the wire version, one
//! discriminator byte and two reserved zero bytes. Decoders consume the whole
//! layout field by field, so a truncated or extended frame can never alias a
//! different frame or carry bytes the relay did not interpret.

use core::{error::Error, fmt};

/// Relay wire version carried by every v2 control frame.
pub const WIRE_VERSION: u8 = 2;

pub type Magic = [u8; 4];

const HEADER_BYTES: usize = 8;

/// Why a received control frame was refused before any authority check.
///
/// The distinction is diagnostic only: peers learn nothing beyond a closed
/// connection, so the variants never become a remote oracle.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum FrameError {
    /// The byte count differs from the frame's exact or declared layout.
    Length,
    /// The magic, wire version or a reserved header byte does not match.
    Header,
    /// The registration mode or challenge purpose is unknown.
    Discriminator,
    /// A semantic field is zero, inconsistent or not an accepted sender key.
    Field,
}

impl fmt::Display for FrameError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::Length => "relay frame length does not match its layout",
            Self::Header => "relay frame header is not this frame's v2 header",
            Self::Discriminator => "relay frame discriminator is unknown",
            Self::Field => "relay frame carries an invalid field",
        })
    }
}

impl Error for FrameError {}

/// Opens a frame whose header byte 5 selects a registration mode or purpose.
pub fn open_discriminated(encoded: &[u8], magic: Magic) -> Result<(u8, Reader<'_>), FrameError> {
    let (header, body) = encoded
        .split_first_chunk::<HEADER_BYTES>()
        .ok_or(FrameError::Length)?;
    let [m0, m1, m2, m3, version, discriminator, reserved @ ..] = *header;
    if [m0, m1, m2, m3] != magic || version != WIRE_VERSION || reserved != [0; 2] {
        return Err(FrameError::Header);
    }
    Ok((discriminator, Reader { rest: body }))
}

/// Opens a frame whose header byte 5 is reserved and must stay zero.
pub fn open_reserved(encoded: &[u8], magic: Magic) -> Result<Reader<'_>, FrameError> {
    match open_discriminated(encoded, magic)? {
        (0, body) => Ok(body),
        _ => Err(FrameError::Header),
    }
}

/// Consumes a frame body in layout order without copying the remainder.
pub struct Reader<'a> {
    rest: &'a [u8],
}

impl<'a> Reader<'a> {
    pub fn array<const N: usize>(&mut self) -> Result<[u8; N], FrameError> {
        let (field, rest) = self
            .rest
            .split_first_chunk::<N>()
            .ok_or(FrameError::Length)?;
        self.rest = rest;
        Ok(*field)
    }

    pub fn u32(&mut self) -> Result<u32, FrameError> {
        self.array().map(u32::from_be_bytes)
    }

    pub const fn remainder(self) -> &'a [u8] {
        self.rest
    }

    /// Rejects trailing bytes after the last field of a fixed layout.
    pub const fn finish(self) -> Result<(), FrameError> {
        if self.rest.is_empty() {
            Ok(())
        } else {
            Err(FrameError::Length)
        }
    }
}

/// Builds frames and signature preimages in a fixed-capacity stack buffer.
///
/// Layouts are compile-time constants, so overflowing the capacity is a
/// programming error that the byte-exact vector tests catch immediately.
pub struct Writer<const N: usize> {
    bytes: [u8; N],
    len: usize,
}

impl<const N: usize> Writer<N> {
    pub const fn new() -> Self {
        Self {
            bytes: [0; N],
            len: 0,
        }
    }

    pub fn with_header(magic: Magic, discriminator: u8) -> Self {
        let mut writer = Self::new();
        writer.put(&magic).put(&[WIRE_VERSION, discriminator, 0, 0]);
        writer
    }

    pub fn put(&mut self, field: &[u8]) -> &mut Self {
        let end = self.len + field.len();
        self.bytes[self.len..end].copy_from_slice(field);
        self.len = end;
        self
    }

    pub fn written(&self) -> &[u8] {
        &self.bytes[..self.len]
    }

    pub fn into_frame(self) -> [u8; N] {
        debug_assert_eq!(self.len, N, "a fixed frame layout must fill its width");
        self.bytes
    }
}
