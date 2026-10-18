//! Atomic authority-row transitions shared by SQL hosts. Hosts own transactions;
//! registration creates ownership and descriptor as one durable fact.
use crate::{
    descriptor_upload::MAX_DESCRIPTOR_BYTES,
    identifiers::{DescriptorDigest, RelayIdentity, ShareId},
    lifecycle::Record,
    share::Storage,
};
use alloc::vec::Vec;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct StoredAuthority {
    record: Record,
    descriptor: Option<Vec<u8>>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    Record,
    Identity,
    Descriptor,
    Conflict,
}

impl StoredAuthority {
    /// Decode and validate an authoritative SQL row before using any of its fields.
    ///
    /// # Errors
    /// Rejects missing/corrupt descriptors and foreign authority.
    pub fn decode(
        relay: RelayIdentity,
        share: ShareId,
        record: &[u8],
        descriptor: Option<Vec<u8>>,
    ) -> Result<Self, Error> {
        let value = Self {
            record: Record::decode(record).map_err(|_| Error::Record)?,
            descriptor,
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
        self.descriptor.as_deref()
    }

    fn validate(&self, relay: RelayIdentity, share: ShareId) -> Result<(), Error> {
        if self.record.relay() != relay || self.record.share() != share {
            return Err(Error::Identity);
        }
        if self
            .descriptor()
            .is_some_and(|bytes| bytes.is_empty() || bytes.len() > MAX_DESCRIPTOR_BYTES)
        {
            return Err(Error::Descriptor);
        }
        let registration = match &self.record {
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
        if matches!(self.record, Record::Stopped(_)) && self.descriptor.is_some() {
            return Err(Error::Descriptor);
        }
        Ok(())
    }
}

/// Compute the next complete row inside the host's serialized transaction.
///
/// # Errors
/// Conflicting ownership never mutates storage. A lost completion is resolved by
/// replaying the exact write, not by assuming failure or deleting the registration.
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
        Storage::Register {
            ownership,
            descriptor,
        } => {
            let next = StoredAuthority {
                record: Record::Published(**ownership),
                descriptor: Some(descriptor.clone()),
            };
            next.validate(relay, share)?;
            // Owner connection lifetimes are never reused. Exact row equality
            // therefore confirms this write without a separate publication key.
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
                    descriptor: if matches!(record, Record::Stopped(_)) {
                        None
                    } else {
                        current.and_then(|row| row.descriptor.clone())
                    },
                };
                value.validate(relay, share)?;
                Ok(value)
            })
            .transpose()
        }
    }
}
