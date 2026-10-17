//! Atomic authority-row transitions shared by SQL hosts. Hosts own transactions;
//! the interpretation of publication, compare-exchange and cleanup is identical.
use crate::{
    descriptor_upload::MAX_DESCRIPTOR_BYTES,
    identifiers::{DescriptorDigest, RelayIdentity, ShareId},
    lifecycle::{OperationId, Record},
    share::Storage,
};
use alloc::vec::Vec;

const OPERATION_KEY_BYTES: usize = 40;

#[derive(Clone, Debug, Eq, PartialEq)]
enum Publication {
    Absent,
    Present { descriptor: Vec<u8>, key: Vec<u8> },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct StoredAuthority {
    record: Record,
    publication: Publication,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    Record,
    Identity,
    Descriptor,
    Publication,
    Conflict,
    Cleanup,
}

impl StoredAuthority {
    /// Decode and validate an authoritative SQL row before using any of its fields.
    ///
    /// # Errors
    /// Rejects partial publications, corrupt descriptors and foreign authority.
    pub fn decode(
        relay: RelayIdentity,
        share: ShareId,
        record: &[u8],
        descriptor: Option<Vec<u8>>,
        publication: Option<Vec<u8>>,
    ) -> Result<Self, Error> {
        let value = Self {
            record: Record::decode(record).map_err(|_| Error::Record)?,
            publication: match (descriptor, publication) {
                (None, None) => Publication::Absent,
                (Some(descriptor), Some(key)) => Publication::Present { descriptor, key },
                _ => return Err(Error::Publication),
            },
        };
        value.validate(relay, share)?;
        Ok(value)
    }

    #[must_use]
    pub const fn record(&self) -> Record {
        self.record
    }

    #[must_use]
    pub fn descriptor(&self) -> Option<&[u8]> {
        match &self.publication {
            Publication::Absent => None,
            Publication::Present { descriptor, .. } => Some(descriptor),
        }
    }

    #[must_use]
    pub fn publication_key(&self) -> Option<&[u8]> {
        match &self.publication {
            Publication::Absent => None,
            Publication::Present { key, .. } => Some(key),
        }
    }

    fn validate(&self, relay: RelayIdentity, share: ShareId) -> Result<(), Error> {
        if self.record.relay() != relay || self.record.share() != share {
            return Err(Error::Identity);
        }
        if let Publication::Present { descriptor, key } = &self.publication
            && (descriptor.is_empty()
                || descriptor.len() > MAX_DESCRIPTOR_BYTES
                || key.len() != OPERATION_KEY_BYTES)
        {
            return Err(Error::Publication);
        }
        let registration = match &self.record {
            Record::Unresolved(record) => Some(record.registration()),
            Record::Published(record) => Some(record.route().registration()),
            Record::Grace(record) => Some(record.route().registration()),
            Record::Stopping(_) | Record::Stopped(_) => None,
        };
        if let Some(registration) = registration
            && self
                .descriptor()
                .is_none_or(|bytes| DescriptorDigest::of(bytes) != registration.descriptor_digest())
        {
            return Err(Error::Descriptor);
        }
        if matches!(self.record, Record::Stopped(_)) && self.descriptor().is_some() {
            return Err(Error::Publication);
        }
        Ok(())
    }
}

/// Compute the next complete row inside the host's serialized transaction.
///
/// # Errors
/// Conflicting ownership and cleanup of another live attempt never mutate storage.
pub fn apply(
    relay: RelayIdentity,
    share: ShareId,
    current: Option<&StoredAuthority>,
    operation: &Storage,
) -> Result<Option<StoredAuthority>, Error> {
    if let Some(current) = current {
        current.validate(relay, share)?;
    }
    match operation {
        Storage::Publish { record, descriptor } => {
            let Record::Unresolved(unresolved) = **record else {
                return Err(Error::Publication);
            };
            let next = StoredAuthority {
                record: **record,
                publication: Publication::Present {
                    descriptor: descriptor.clone(),
                    key: operation_key(unresolved.id()),
                },
            };
            next.validate(relay, share)?;
            if current.is_some_and(|row| row != &next) {
                return Err(Error::Conflict);
            }
            Ok(Some(next))
        }
        Storage::CompareExchange { expected, next } => {
            let actual = current.map(StoredAuthority::record);
            let next = next.as_deref().copied();
            if actual != expected.as_deref().copied() && actual != next {
                return Err(Error::Conflict);
            }
            next.map(|record| {
                let value = StoredAuthority {
                    record,
                    publication: if matches!(record, Record::Stopped(_)) {
                        Publication::Absent
                    } else {
                        current.map_or(Publication::Absent, |row| row.publication.clone())
                    },
                };
                value.validate(relay, share)?;
                Ok(value)
            })
            .transpose()
        }
        Storage::RemovePublication(id) => {
            if id.share() != share || id.relay() != relay {
                return Err(Error::Identity);
            }
            let Some(current) = current else {
                return Ok(None);
            };
            if matches!(current.record, Record::Unresolved(record) if record.id() == *id) {
                return Ok(None);
            }
            let mut next = current.clone();
            if current.publication_key() == Some(operation_key(*id).as_slice()) {
                if !matches!(current.record, Record::Stopping(_)) {
                    return Err(Error::Cleanup);
                }
                next.publication = Publication::Absent;
            }
            Ok(Some(next))
        }
    }
}

fn operation_key(operation: OperationId) -> Vec<u8> {
    let mut bytes = Vec::with_capacity(OPERATION_KEY_BYTES);
    bytes.extend_from_slice(operation.incarnation().as_bytes());
    bytes.extend_from_slice(&operation.connection().id.get().to_be_bytes());
    bytes.extend_from_slice(&operation.connection().generation.get().to_be_bytes());
    bytes.extend_from_slice(&operation.sequence().get().to_be_bytes());
    bytes
}
