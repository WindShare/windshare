//! Recovery completeness cannot be inferred from the sockets a host can still see.
use super::checkpoint_codec::{bounded_count, field, put_header, read_header};
use super::{ConnectionRef, Error, Identity, Limits, Observer, ShareRelay};
use crate::wire::Reader;
use alloc::vec::Vec;

const MANIFEST_FORMAT: u8 = 1;

/// Complete retained-generation inventory, stored independently of socket attachments.
///
/// Hosts must preserve this and the latest records as one recoverable state. A
/// missing manifest is unknown responsibility, including when no sockets survive.
/// This is an ownership contract with the host, not proof against stale storage.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CheckpointManifest {
    pub(super) identity: Identity,
    pub(super) last_session: u64,
    pub(super) generations: Vec<ConnectionRef>,
}

impl CheckpointManifest {
    #[must_use]
    pub fn generations(&self) -> &[ConnectionRef] {
        &self.generations
    }

    #[must_use]
    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::new();
        out.push(MANIFEST_FORMAT);
        put_header(&mut out, self.identity, self.last_session);
        out.extend_from_slice(&(self.generations.len() as u64).to_be_bytes());
        for connection in &self.generations {
            out.extend_from_slice(&connection.encode());
        }
        out
    }

    /// # Errors
    /// Rejects malformed, duplicate, unordered or over-capacity inventories.
    pub fn decode(bytes: &[u8], limits: Limits) -> Result<Self, Error> {
        let mut body = Reader::new(bytes);
        if field::<1>(&mut body)? != [MANIFEST_FORMAT] {
            return Err(Error::InvalidCheckpoint);
        }
        let (identity, last_session) = read_header(&mut body)?;
        let count = bounded_count(&mut body, limits.generation_limit()?)?;
        let mut generations = Vec::with_capacity(count);
        for _ in 0..count {
            generations
                .push(ConnectionRef::decode(field(&mut body)?).ok_or(Error::InvalidCheckpoint)?);
        }
        body.finish().map_err(|_| Error::InvalidCheckpoint)?;
        let manifest = Self {
            identity,
            last_session,
            generations,
        };
        manifest.validate(limits)?;
        Ok(manifest)
    }

    pub(super) fn validate(&self, limits: Limits) -> Result<(), Error> {
        if self.generations.len() > limits.generation_limit()?
            || self.generations.windows(2).any(|pair| pair[0] >= pair[1])
        {
            return Err(Error::InvalidCheckpoint);
        }
        Ok(())
    }
}

impl<O: Observer> ShareRelay<O> {
    /// Capture the complete inventory without copying delivery ledgers.
    ///
    /// # Errors
    /// Unfinished durable lifecycle work must complete before capturing recovery state.
    pub fn checkpoint_manifest(&self) -> Result<CheckpointManifest, Error> {
        self.checkpoint_ready()?;
        Ok(CheckpointManifest {
            identity: self.identity,
            last_session: self.last_session,
            generations: self.ledgers.keys().copied().collect(),
        })
    }
}
