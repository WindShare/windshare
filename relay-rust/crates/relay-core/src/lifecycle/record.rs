//! Durable share records: the only facts a host persists, and the only input
//! that constructs a share lifecycle.
//!
//! Reservations never reach storage. A publication transaction stores its
//! unresolved record before it can commit, and REGISTERED first replaces that
//! record with the published route.
//! A crash at any point therefore restores a fenced attempt that still needs
//! reconciliation, or a route whose sender may already hold REGISTERED, never
//! a revived attempt the lifecycle fenced.
//!
//! Records hold no deadline yet. Any later deadline is stored as an absolute
//! instant, so restoring never extends it.

use core::{error::Error, fmt, num::NonZeroU64};

use crate::{
    identifiers::{
        DescriptorDigest, RelayIdentity, ResumeTokenHash, ShareId, ShareInstance, ShareRoute,
        StopId,
    },
    registration::Registration,
    wire::{Reader, Writer},
};

use super::{ConnectionRef, Incarnation, RegistrationId};

/// Persisted layout version. Decoders refuse every other value, so a future
/// layout can never be misread as this one.
const FORMAT: u8 = 1;

const UNRESOLVED: u8 = 1;
const PUBLISHED: u8 = 2;
const STOPPED: u8 = 3;

// Format and kind, then the relay identity and share ID every record names.
const HEADER_BYTES: usize = 1 + 1 + 32 + 12;
// Instance, key hash, descriptor digest and resume commitment: the WS2R body
// after its share ID, which the header already holds.
const REGISTRATION_BYTES: usize = 16 + 16 + 32 + 32;
// Incarnation, connection slot and generation, then the attempt sequence.
const OPERATION_BYTES: usize = 16 + 8 + 8 + 8;

/// Largest encoded record, an unresolved publication; hosts may size storage
/// columns from it.
pub const MAX_RECORD_BYTES: usize = HEADER_BYTES + REGISTRATION_BYTES + OPERATION_BYTES;

/// Durable ownership epoch of one published route.
///
/// Restoration preserves it. RESUME and post-hibernation re-adoption will
/// compare it, so an owner that a newer epoch superseded cannot reattach.
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub struct RouteVersion(NonZeroU64);

impl RouteVersion {
    /// The epoch a fresh publication starts.
    pub const FIRST: Self = Self(NonZeroU64::MIN);

    /// Returns the epoch number.
    #[must_use]
    pub const fn get(self) -> NonZeroU64 {
        self.0
    }
}

/// A publication whose transaction may have committed.
///
/// Neither a timeout nor a restart may forget it: only reconciliation of this
/// exact operation, ending in `PublicationResult::Aborted`, removes it.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct UnresolvedPublication {
    pub(super) id: RegistrationId,
    pub(super) registration: Registration,
}

impl UnresolvedPublication {
    /// Returns the operation whose resources and record need reconciliation.
    #[must_use]
    pub const fn id(&self) -> RegistrationId {
        self.id
    }

    /// Returns the registration the transaction may have stored.
    #[must_use]
    pub const fn registration(&self) -> &Registration {
        &self.registration
    }
}

/// A registration the lifecycle acknowledged with REGISTERED.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PublishedRoute {
    pub(super) relay: RelayIdentity,
    pub(super) registration: Registration,
    pub(super) version: RouteVersion,
}

impl PublishedRoute {
    /// Returns the published facts, including the resume commitment.
    #[must_use]
    pub const fn registration(&self) -> &Registration {
        &self.registration
    }

    /// Returns the ownership epoch hosts record beside the owning connection.
    #[must_use]
    pub const fn version(&self) -> RouteVersion {
        self.version
    }
}

/// A share instance its sender permanently retired on this relay.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct StoppedShare {
    pub(super) relay: RelayIdentity,
    pub(super) route: ShareRoute,
    pub(super) share_instance: ShareInstance,
    pub(super) stop_id: StopId,
}

impl StoppedShare {
    /// Returns the retired route.
    #[must_use]
    pub const fn route(&self) -> ShareRoute {
        self.route
    }

    /// Returns the content instance that can never publish or resume again.
    #[must_use]
    pub const fn share_instance(&self) -> ShareInstance {
        self.share_instance
    }

    /// Returns the identifier of the STOP that retired the instance.
    #[must_use]
    pub const fn stop_id(&self) -> StopId {
        self.stop_id
    }
}

/// Everything a host stores for one share on one relay.
///
/// Hosts key records by relay identity and share ID and store exactly the
/// records that lifecycle outputs name. Absence means the share is vacant.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Record {
    /// Stored by the publication transaction, from `PublishRequest::record`.
    Unresolved(UnresolvedPublication),
    /// Replaces the unresolved record before REGISTERED, from
    /// `PublishedRegistration::record`.
    Published(PublishedRoute),
    /// Stored by STOP before STOPPED; never removed.
    Stopped(StoppedShare),
}

impl Record {
    /// Returns the relay whose storage owns this record.
    #[must_use]
    pub const fn relay(&self) -> RelayIdentity {
        match self {
            Self::Unresolved(unresolved) => unresolved.id.relay,
            Self::Published(route) => route.relay,
            Self::Stopped(stopped) => stopped.relay,
        }
    }

    /// Returns the share this record belongs to.
    #[must_use]
    pub const fn share(&self) -> ShareId {
        match self {
            Self::Unresolved(unresolved) => unresolved.registration.route().share_id(),
            Self::Published(route) => route.registration.route().share_id(),
            Self::Stopped(stopped) => stopped.route.share_id(),
        }
    }

    /// Encodes the bytes a host persists, at most [`MAX_RECORD_BYTES`].
    #[must_use]
    pub fn encode(&self) -> EncodedRecord {
        let kind = match self {
            Self::Unresolved(_) => UNRESOLVED,
            Self::Published(_) => PUBLISHED,
            Self::Stopped(_) => STOPPED,
        };
        let mut out = Writer::new();
        out.put(&[FORMAT, kind])
            .put(self.relay().as_bytes())
            .put(self.share().as_bytes());
        match self {
            Self::Unresolved(unresolved) => {
                put_registration(&mut out, &unresolved.registration);
                put_operation(&mut out, unresolved.id);
            }
            Self::Published(route) => {
                put_registration(&mut out, &route.registration);
                out.put(&route.version.get().get().to_be_bytes());
            }
            Self::Stopped(stopped) => {
                out.put(stopped.share_instance.as_bytes())
                    .put(stopped.route.pk_hash().as_bytes())
                    .put(stopped.stop_id.as_bytes());
            }
        }
        EncodedRecord(out)
    }

    /// Decodes bytes a host persisted from [`Self::encode`].
    ///
    /// # Errors
    ///
    /// Returns a [`RecordError`] unless the bytes have this format's exact
    /// layout for a known kind, nonzero identities, and a share ID derived from
    /// the stored key hash.
    pub fn decode(encoded: &[u8]) -> Result<Self, RecordError> {
        let mut body = Reader::new(encoded);
        let [format, kind] = take(&mut body)?;
        if format != FORMAT {
            return Err(RecordError::Format);
        }
        let relay = RelayIdentity::new(take(&mut body)?).ok_or(RecordError::Field)?;
        let share_id = take(&mut body)?;
        let record = match kind {
            UNRESOLVED => {
                let registration = read_registration(&mut body, share_id)?;
                Self::Unresolved(UnresolvedPublication {
                    id: read_operation(&mut body, relay, registration.route().share_id())?,
                    registration,
                })
            }
            PUBLISHED => Self::Published(PublishedRoute {
                relay,
                registration: read_registration(&mut body, share_id)?,
                version: RouteVersion(nonzero(&mut body)?),
            }),
            STOPPED => {
                let share_instance = instance(&mut body)?;
                Self::Stopped(StoppedShare {
                    relay,
                    route: route(share_id, take(&mut body)?)?,
                    share_instance,
                    stop_id: StopId::new(take(&mut body)?).ok_or(RecordError::Field)?,
                })
            }
            _ => return Err(RecordError::Kind),
        };
        body.finish().map_err(|_| RecordError::Length)?;
        Ok(record)
    }
}

/// The persisted bytes of one record; their length depends on its kind.
pub struct EncodedRecord(Writer<MAX_RECORD_BYTES>);

impl EncodedRecord {
    /// Returns the bytes to store.
    #[must_use]
    pub fn as_bytes(&self) -> &[u8] {
        self.0.written()
    }
}

impl fmt::Debug for EncodedRecord {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_tuple("EncodedRecord")
            .field(&self.as_bytes())
            .finish()
    }
}

/// Why stored bytes are not a record this kernel wrote.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RecordError {
    /// The byte count differs from the record kind's exact layout.
    Length,
    /// The bytes use a layout version this kernel does not read.
    Format,
    /// The record kind is unknown.
    Kind,
    /// An identity is zero, or a share ID does not derive from its key hash.
    Field,
}

impl fmt::Display for RecordError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::Length => "lifecycle record length does not match its layout",
            Self::Format => "lifecycle record uses an unknown layout version",
            Self::Kind => "lifecycle record kind is unknown",
            Self::Field => "lifecycle record carries an invalid field",
        })
    }
}

impl Error for RecordError {}

/// Why a stored record cannot construct a share's lifecycle.
///
/// The record may be the only evidence of a published route or a permanent
/// STOP, so hosts must never substitute a vacant share for it.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RestoreError {
    /// The record belongs to another relay identity's storage.
    ForeignRelay,
    /// The record belongs to another share.
    ForeignShare,
    /// The incarnation minted the stored operation, so callbacks from before
    /// the restore could name new attempts.
    ReusedIncarnation,
}

impl fmt::Display for RestoreError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::ForeignRelay => "lifecycle record belongs to another relay",
            Self::ForeignShare => "lifecycle record belongs to another share",
            Self::ReusedIncarnation => "restoring incarnation minted the stored operation",
        })
    }
}

impl Error for RestoreError {}

fn put_registration(out: &mut Writer<MAX_RECORD_BYTES>, registration: &Registration) {
    out.put(registration.share_instance().as_bytes())
        .put(registration.route().pk_hash().as_bytes())
        .put(registration.descriptor_digest().as_bytes())
        .put(registration.resume_token_hash().as_bytes());
}

fn put_operation(out: &mut Writer<MAX_RECORD_BYTES>, id: RegistrationId) {
    out.put(id.incarnation.as_bytes())
        .put(&id.connection.id.get().to_be_bytes())
        .put(&id.connection.generation.get().to_be_bytes())
        .put(&id.sequence.get().to_be_bytes());
}

fn take<const N: usize>(body: &mut Reader<'_>) -> Result<[u8; N], RecordError> {
    body.array().map_err(|_| RecordError::Length)
}

fn nonzero(body: &mut Reader<'_>) -> Result<NonZeroU64, RecordError> {
    NonZeroU64::new(u64::from_be_bytes(take(body)?)).ok_or(RecordError::Field)
}

fn instance(body: &mut Reader<'_>) -> Result<ShareInstance, RecordError> {
    ShareInstance::new(take(body)?).ok_or(RecordError::Field)
}

fn route(share_id: [u8; 12], pk_hash: [u8; 16]) -> Result<ShareRoute, RecordError> {
    ShareRoute::from_wire(share_id, pk_hash).map_err(|_| RecordError::Field)
}

fn read_registration(
    body: &mut Reader<'_>,
    share_id: [u8; 12],
) -> Result<Registration, RecordError> {
    let share_instance = instance(body)?;
    let route = route(share_id, take(body)?)?;
    let descriptor_digest = DescriptorDigest::new(take(body)?);
    let resume_token_hash = ResumeTokenHash::new(take(body)?);
    Ok(Registration::new(
        route,
        share_instance,
        descriptor_digest,
        resume_token_hash,
    ))
}

fn read_operation(
    body: &mut Reader<'_>,
    relay: RelayIdentity,
    share: ShareId,
) -> Result<RegistrationId, RecordError> {
    let incarnation = Incarnation::new(take(body)?).ok_or(RecordError::Field)?;
    let connection = ConnectionRef {
        id: nonzero(body)?,
        generation: nonzero(body)?,
    };
    Ok(RegistrationId {
        relay,
        share,
        incarnation,
        connection,
        sequence: nonzero(body)?,
    })
}

#[cfg(test)]
#[path = "record_tests.rs"]
mod tests;
