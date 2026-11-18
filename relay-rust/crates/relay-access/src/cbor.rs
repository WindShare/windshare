//! A deliberately small canonical CBOR vocabulary for signed authority records.
//! Definite arrays and shortest unsigned integers eliminate cross-runtime ambiguity.
use alloc::vec::Vec;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    Encoding,
    Signature,
    Expired,
    Untrusted,
    Rollback,
    Lifetime,
}

pub struct Encoder(pub Vec<u8>);

impl Encoder {
    pub const fn new() -> Self {
        Self(Vec::new())
    }
    pub fn array(&mut self, length: u64) {
        self.head(4, length);
    }
    pub fn uint(&mut self, value: u64) {
        self.head(0, value);
    }
    pub fn bytes(&mut self, value: &[u8]) {
        self.head(2, value.len() as u64);
        self.0.extend_from_slice(value);
    }
    pub fn text(&mut self, value: &str) {
        self.head(3, value.len() as u64);
        self.0.extend_from_slice(value.as_bytes());
    }
    fn head(&mut self, major: u8, value: u64) {
        let prefix = major << 5;
        if value < 24 {
            self.0
                .push(prefix | u8::try_from(value).expect("small integer"));
        } else if let Ok(value) = u8::try_from(value) {
            self.0.extend_from_slice(&[prefix | 0x18, value]);
        } else if let Ok(value) = u16::try_from(value) {
            self.0.push(prefix | 0x19);
            self.0.extend_from_slice(&value.to_be_bytes());
        } else if let Ok(value) = u32::try_from(value) {
            self.0.push(prefix | 0x1a);
            self.0.extend_from_slice(&value.to_be_bytes());
        } else {
            self.0.push(prefix | 0x1b);
            self.0.extend_from_slice(&value.to_be_bytes());
        }
    }
}

pub struct Decoder<'a>(&'a [u8]);

impl<'a> Decoder<'a> {
    pub const fn new(bytes: &'a [u8]) -> Self {
        Self(bytes)
    }
    pub fn array(&mut self, length: u64) -> Result<(), Error> {
        if self.head(4)? != length {
            return Err(Error::Encoding);
        }
        Ok(())
    }
    pub fn uint(&mut self) -> Result<u64, Error> {
        self.head(0)
    }
    pub fn bytes<const N: usize>(&mut self) -> Result<[u8; N], Error> {
        if self.head(2)? != N as u64 {
            return Err(Error::Encoding);
        }
        self.take(N)?.try_into().map_err(|_| Error::Encoding)
    }
    pub fn text(&mut self, expected: &str) -> Result<(), Error> {
        if self.head(3)? != expected.len() as u64
            || self.take(expected.len())? != expected.as_bytes()
        {
            return Err(Error::Encoding);
        }
        Ok(())
    }
    pub const fn end(self) -> Result<(), Error> {
        if self.0.is_empty() {
            Ok(())
        } else {
            Err(Error::Encoding)
        }
    }
    fn take(&mut self, size: usize) -> Result<&'a [u8], Error> {
        let (value, rest) = self.0.split_at_checked(size).ok_or(Error::Encoding)?;
        self.0 = rest;
        Ok(value)
    }
    fn head(&mut self, major: u8) -> Result<u64, Error> {
        let tag = self.take(1)?[0];
        if tag >> 5 != major {
            return Err(Error::Encoding);
        }
        let extra = tag & 31;
        let (size, minimum) = match extra {
            0..=23 => return Ok(u64::from(extra)),
            24 => (1, 24),
            25 => (2, 256),
            26 => (4, 65_536),
            27 => (8, 4_294_967_296),
            _ => return Err(Error::Encoding),
        };
        let value = self
            .take(size)?
            .iter()
            .fold(0u64, |value, byte| (value << 8) | u64::from(*byte));
        if value < minimum {
            return Err(Error::Encoding);
        }
        Ok(value)
    }
}
