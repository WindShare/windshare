//! Connection-local probes. Clients originate them; relays only acknowledge.

use crate::{
    clock::UnixMillis,
    wire::{self, FrameError, Writer},
};
use core::{num::NonZeroU64, time::Duration};

pub const CONNECTION_PROBE_INTERVAL: Duration = Duration::from_secs(15);
// Three client intervals tolerate a bounded write plus network delay. An individual
// queued acknowledgement is never a separate failure timer.
pub const CONNECTION_SILENCE_TIMEOUT: Duration = Duration::from_secs(45);
const FRAME_BYTES: usize = wire::HEADER_BYTES + size_of::<u64>();

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ConnectionProbe(NonZeroU64);

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ConnectionProbeAck(NonZeroU64);

impl ConnectionProbe {
    pub const MAGIC: [u8; 4] = *b"WS2H";

    #[must_use]
    pub const fn new(nonce: NonZeroU64) -> Self {
        Self(nonce)
    }

    #[must_use]
    pub const fn nonce(self) -> u64 {
        self.0.get()
    }

    #[must_use]
    pub const fn acknowledgement(self) -> ConnectionProbeAck {
        ConnectionProbeAck(self.0)
    }

    #[must_use]
    pub fn encode(self) -> [u8; FRAME_BYTES] {
        encode_nonce(Self::MAGIC, self.0)
    }

    /// # Errors
    /// Refuses malformed headers, trailing bytes and zero nonces.
    pub fn decode(bytes: &[u8]) -> Result<Self, FrameError> {
        decode_nonce(bytes, Self::MAGIC).map(Self)
    }
}

impl ConnectionProbeAck {
    pub const MAGIC: [u8; 4] = *b"WS2A";

    #[must_use]
    pub const fn nonce(self) -> u64 {
        self.0.get()
    }

    #[must_use]
    pub fn encode(self) -> [u8; FRAME_BYTES] {
        encode_nonce(Self::MAGIC, self.0)
    }

    /// # Errors
    /// Refuses malformed headers, trailing bytes and zero nonces.
    pub fn decode(bytes: &[u8]) -> Result<Self, FrameError> {
        decode_nonce(bytes, Self::MAGIC).map(Self)
    }
}

fn encode_nonce(magic: [u8; 4], nonce: NonZeroU64) -> [u8; FRAME_BYTES] {
    let mut writer = Writer::with_header(magic, 0);
    writer.put(&nonce.get().to_be_bytes());
    writer.into_frame()
}

fn decode_nonce(bytes: &[u8], magic: [u8; 4]) -> Result<NonZeroU64, FrameError> {
    let mut body = wire::open_reserved(bytes, magic)?;
    let nonce = u64::from_be_bytes(body.array()?);
    body.finish()?;
    NonZeroU64::new(nonce).ok_or(FrameError::Field)
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct Liveness {
    last_probe_nonce: u64,
    last_inbound: UnixMillis,
    deadline: UnixMillis,
}

impl Liveness {
    pub const ENCODED_BYTES: usize = 3 * size_of::<u64>();

    pub fn established(now: UnixMillis) -> Self {
        Self {
            last_probe_nonce: 0,
            last_inbound: now,
            deadline: now.saturating_add(CONNECTION_SILENCE_TIMEOUT),
        }
    }

    pub const fn last_probe_nonce(self) -> u64 {
        self.last_probe_nonce
    }

    pub const fn last_inbound(self) -> UnixMillis {
        self.last_inbound
    }

    pub const fn deadline(self) -> UnixMillis {
        self.deadline
    }

    pub const fn accept_probe(&mut self, probe: ConnectionProbe) -> bool {
        if probe.nonce() <= self.last_probe_nonce {
            return false;
        }
        self.last_probe_nonce = probe.nonce();
        true
    }

    pub fn accepted_inbound(&mut self, now: UnixMillis) -> bool {
        // Clock corrections must not move the recorded evidence backwards.
        if now <= self.last_inbound {
            return false;
        }
        self.last_inbound = now;
        self.deadline = now.saturating_add(CONNECTION_SILENCE_TIMEOUT);
        true
    }

    pub fn encode(self) -> [u8; Self::ENCODED_BYTES] {
        let mut writer = Writer::new();
        writer
            .put(&self.last_probe_nonce.to_be_bytes())
            .put(&self.last_inbound.0.to_be_bytes())
            .put(&self.deadline.0.to_be_bytes());
        writer.into_frame()
    }

    pub fn decode(bytes: &[u8]) -> Result<Self, FrameError> {
        let mut body = wire::Reader::new(bytes);
        let last_probe_nonce = u64::from_be_bytes(body.array()?);
        let last_inbound = UnixMillis(u64::from_be_bytes(body.array()?));
        let deadline = UnixMillis(u64::from_be_bytes(body.array()?));
        body.finish()?;
        if deadline != last_inbound.saturating_add(CONNECTION_SILENCE_TIMEOUT) {
            return Err(FrameError::Field);
        }
        Ok(Self {
            last_probe_nonce,
            last_inbound,
            deadline,
        })
    }
}

#[cfg(test)]
#[path = "heartbeat_tests.rs"]
mod tests;
