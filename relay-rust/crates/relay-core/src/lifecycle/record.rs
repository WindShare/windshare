//! Durable share records: the only facts a host persists, and the only input
//! that constructs a share lifecycle.
//!
//! Reservations and resume attempts never reach storage. REGISTER creates the
//! complete ownership and descriptor atomically; RESUME replaces that ownership.
//! The runtime waits for exact commit evidence before service or REGISTERED.
//! An authoritative read after a crash can establish commitment even when the
//! completion or response was lost. Surviving owners keep their sessions;
//! absent owners enter grace instead of rolling back a committed registration.
//!
//! Losing the owner stores the lost ownership and an absolute grace deadline.
//! Only a restore that finds the stored owner gone derives one, so later
//! restores never extend it. STOP first persists its permanent fact together
//! with the exact cleanup obligation, then replaces it with Stopped only after
//! cleanup. Restoring Stopping repeats cleanup; it never re-adopts survivors.

use core::{error::Error, fmt, num::NonZeroU64};

use crate::{
    clock::UnixMillis,
    identifiers::{
        DescriptorDigest, RelayIdentity, ResumeTokenHash, ShareId, ShareInstance, ShareRoute,
        StopId,
    },
    registration::Registration,
    wire::{Reader, Writer},
};

use super::{
    ConnectionRef, Incarnation, OperationId, OwnershipEpoch, OwnershipId, PendingStop, StopCleanup,
};

/// Persisted layout version. Decoders refuse every other value, so a future
/// layout can never be misread as this one.
const FORMAT: u8 = 6;

const PUBLISHED: u8 = 2;
const GRACE: u8 = 3;
const STOPPED: u8 = 4;
const STOPPING: u8 = 5;

const CLEANUP_NONE: u8 = 0;
const CLEANUP_RESERVATION: u8 = 1;
const CLEANUP_OWNER: u8 = 3;

// Format and kind, then the relay identity and share ID every record names.
const HEADER_BYTES: usize = 1 + 1 + 32 + 12;
// Instance, key hash, descriptor digest and resume commitment: the WS2R body
// after its share ID, which the header already holds.
const REGISTRATION_BYTES: usize = 16 + 16 + 32 + 32 + crate::access::Attribution::ENCODED_BYTES;
// Connection slot and generation.
const CONNECTION_BYTES: usize = ConnectionRef::ENCODED_BYTES;
// Incarnation and connection, then the attempt sequence.
const OPERATION_BYTES: usize = 16 + CONNECTION_BYTES + 8;
// Epoch, then the owner connection.
const OWNERSHIP_BYTES: usize = 8 + CONNECTION_BYTES;
// Grace retains the registration, deadline and lost ownership.
const GRACE_BYTES: usize = HEADER_BYTES + REGISTRATION_BYTES + 8 + OWNERSHIP_BYTES;
// STOP with a reservation is one byte larger: both operation identities survive.
const STOP_FACT_BYTES: usize = 16 + 16 + 16;
const STOPPING_BYTES: usize =
    HEADER_BYTES + STOP_FACT_BYTES + OPERATION_BYTES + 1 + 1 + OPERATION_BYTES;

/// Largest encoded record, a STOP retaining a reserved handshake.
pub const MAX_RECORD_BYTES: usize = if GRACE_BYTES > STOPPING_BYTES {
    GRACE_BYTES
} else {
    STOPPING_BYTES
};

/// A durably committed registration, even if REGISTERED was lost. Every owner
/// that resumes it serves these same facts.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PublishedRoute {
    pub(super) relay: RelayIdentity,
    pub(super) registration: Registration,
    pub(super) attribution: crate::access::Attribution,
}

impl PublishedRoute {
    #[must_use]
    pub const fn attribution(&self) -> crate::access::Attribution {
        self.attribution
    }

    /// Returns the published facts, including the resume commitment.
    #[must_use]
    pub const fn registration(&self) -> &Registration {
        &self.registration
    }

    pub(super) const fn ownership(
        &self,
        epoch: OwnershipEpoch,
        connection: ConnectionRef,
    ) -> OwnershipId {
        OwnershipId::new(
            self.relay,
            self.registration.route().share_id(),
            epoch,
            connection,
        )
    }
}

/// A committed route and the ownership entitled to serve it after confirmation.
///
/// Naming the ownership lets a restore re-adopt an owner that survived, such
/// as a hibernated socket, without writing or changing the ownership its
/// sessions are bound to; an owner that did not survive starts grace instead.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct OwnedRoute {
    pub(super) route: PublishedRoute,
    pub(super) epoch: OwnershipEpoch,
    pub(super) owner: ConnectionRef,
}

impl OwnedRoute {
    /// Returns the published route.
    #[must_use]
    pub const fn route(&self) -> &PublishedRoute {
        &self.route
    }

    /// Returns the ownership entitled to serve the route after confirmation.
    #[must_use]
    pub const fn ownership(&self) -> OwnershipId {
        self.route.ownership(self.epoch, self.owner)
    }
}

/// A published route that no connection serves. It lapses at the absolute
/// deadline unless its sender resumes first.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct GraceRoute {
    pub(super) route: PublishedRoute,
    pub(super) deadline: UnixMillis,
    // The lost ownership: a resume that observed it, even before a restore,
    // still replaces it, and the next owner continues its epochs.
    pub(super) epoch: OwnershipEpoch,
    pub(super) owner: ConnectionRef,
}

impl GraceRoute {
    pub(super) const fn new(
        route: PublishedRoute,
        lost: OwnershipId,
        deadline: UnixMillis,
    ) -> Self {
        Self {
            route,
            deadline,
            epoch: lost.epoch,
            owner: lost.connection,
        }
    }

    /// Returns the published route.
    #[must_use]
    pub const fn route(&self) -> &PublishedRoute {
        &self.route
    }

    /// Returns the ownership the route lost.
    #[must_use]
    pub const fn lost(&self) -> OwnershipId {
        self.route.ownership(self.epoch, self.owner)
    }

    /// Returns the instant the route lapses, persisted so restores never
    /// extend it.
    #[must_use]
    pub const fn deadline(&self) -> UnixMillis {
        self.deadline
    }

    /// The record that replaces the share's owned record.
    #[must_use]
    pub const fn record(self) -> Record {
        Record::Grace(self)
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
    /// The final record, stored only after exact participant cleanup.
    #[must_use]
    pub const fn record(self) -> Record {
        Record::Stopped(self)
    }

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
    /// Committed ownership, created with its descriptor by REGISTER or replaced
    /// by RESUME. Runtime service still waits for exact durable confirmation.
    Published(OwnedRoute),
    /// Replaces the owned record once its owner is gone, from
    /// `GraceRoute::record`; removed when the route lapses.
    Grace(GraceRoute),
    /// Stored before STOP cleanup, retaining its exact participants across
    /// restart. It may become Stopped, never a live route or an absent record.
    Stopping(PendingStop),
    /// Stored after the STOP revocation barrier and before STOPPED; never removed.
    /// This record does not assert that delivery occupancy has been settled.
    Stopped(StoppedShare),
}

impl Record {
    /// Returns the relay whose storage owns this record.
    #[must_use]
    pub const fn relay(&self) -> RelayIdentity {
        match self {
            Self::Published(owned) => owned.route.relay,
            Self::Grace(grace) => grace.route.relay,
            Self::Stopping(pending) => pending.id().relay(),
            Self::Stopped(stopped) => stopped.relay,
        }
    }

    /// Returns the share this record belongs to.
    #[must_use]
    pub const fn share(&self) -> ShareId {
        match self {
            Self::Published(owned) => owned.route.registration.route().share_id(),
            Self::Grace(grace) => grace.route.registration.route().share_id(),
            Self::Stopping(pending) => pending.id().share(),
            Self::Stopped(stopped) => stopped.route.share_id(),
        }
    }

    /// Encodes the bytes a host persists, at most [`MAX_RECORD_BYTES`].
    #[must_use]
    pub fn encode(&self) -> EncodedRecord {
        let kind = match self {
            Self::Published(_) => PUBLISHED,
            Self::Grace(_) => GRACE,
            Self::Stopping(_) => STOPPING,
            Self::Stopped(_) => STOPPED,
        };
        let mut out = Writer::new();
        out.put(&[FORMAT, kind])
            .put(self.relay().as_bytes())
            .put(self.share().as_bytes());
        match self {
            Self::Published(owned) => {
                put_registration(&mut out, &owned.route.registration);
                out.put(&owned.route.attribution.encode());
                put_ownership(&mut out, owned.epoch, owned.owner);
            }
            Self::Grace(grace) => {
                put_registration(&mut out, &grace.route.registration);
                out.put(&grace.route.attribution.encode());
                out.put(&grace.deadline.0.to_be_bytes());
                put_ownership(&mut out, grace.epoch, grace.owner);
            }
            Self::Stopping(pending) => {
                put_stopped(&mut out, pending.stopped());
                put_operation(&mut out, pending.id);
                out.put(&[match pending.origin {
                    super::stopping::StopOrigin::Sender => 0,
                    super::stopping::StopOrigin::Administrator => 1,
                }]);
                put_cleanup(&mut out, pending.cleanup);
            }
            Self::Stopped(stopped) => put_stopped(&mut out, *stopped),
        }
        EncodedRecord(out)
    }

    /// Decodes bytes a host persisted from [`Self::encode`].
    ///
    /// # Errors
    ///
    /// Returns a [`RecordError`] unless the bytes have this format's exact
    /// layout for a known kind, nonzero identities and connection references,
    /// and a share ID derived from the stored key hash.
    pub fn decode(encoded: &[u8]) -> Result<Self, RecordError> {
        let mut body = Reader::new(encoded);
        let [format, kind] = take(&mut body)?;
        if format != FORMAT {
            return Err(RecordError::Format);
        }
        let relay = RelayIdentity::new(take(&mut body)?).ok_or(RecordError::Field)?;
        let share_id = take(&mut body)?;
        let record = match kind {
            PUBLISHED => Self::Published(OwnedRoute {
                route: read_route(&mut body, relay, share_id)?,
                epoch: OwnershipEpoch::new(nonzero(&mut body)?),
                owner: read_connection(&mut body)?,
            }),
            GRACE => Self::Grace(GraceRoute {
                route: read_route(&mut body, relay, share_id)?,
                deadline: UnixMillis(u64::from_be_bytes(take(&mut body)?)),
                epoch: OwnershipEpoch::new(nonzero(&mut body)?),
                owner: read_connection(&mut body)?,
            }),
            STOPPING => {
                let stopped = read_stopped(&mut body, relay, share_id)?;
                let share = stopped.route.share_id();
                let id = read_operation(&mut body, relay, share)?;
                let origin = match take::<1>(&mut body)?[0] {
                    0 => super::stopping::StopOrigin::Sender,
                    1 => super::stopping::StopOrigin::Administrator,
                    _ => return Err(RecordError::Field),
                };
                let mut pending =
                    PendingStop::new(id, stopped, read_cleanup(&mut body, relay, share)?);
                pending.origin = origin;
                Self::Stopping(pending)
            }
            STOPPED => Self::Stopped(read_stopped(&mut body, relay, share_id)?),
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

/// Why stored bytes are not a lifecycle record or session snapshot this
/// kernel wrote.
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
            Self::Length => "stored relay record length does not match its layout",
            Self::Format => "stored relay record uses an unknown layout version",
            Self::Kind => "stored relay record kind is unknown",
            Self::Field => "stored relay record carries an invalid field",
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

fn put_stopped(out: &mut Writer<MAX_RECORD_BYTES>, stopped: StoppedShare) {
    out.put(stopped.share_instance.as_bytes())
        .put(stopped.route.pk_hash().as_bytes())
        .put(stopped.stop_id.as_bytes());
}

fn put_cleanup(out: &mut Writer<MAX_RECORD_BYTES>, cleanup: StopCleanup) {
    match cleanup {
        StopCleanup::None => {
            out.put(&[CLEANUP_NONE]);
        }
        StopCleanup::Reservation(id) => {
            out.put(&[CLEANUP_RESERVATION]);
            put_operation(out, id);
        }
        StopCleanup::Owner(owner) => {
            out.put(&[CLEANUP_OWNER]);
            put_connection(out, owner);
        }
    }
}

fn read_stopped(
    body: &mut Reader<'_>,
    relay: RelayIdentity,
    share_id: [u8; 12],
) -> Result<StoppedShare, RecordError> {
    let share_instance = instance(body)?;
    Ok(StoppedShare {
        relay,
        route: route(share_id, take(body)?)?,
        share_instance,
        stop_id: StopId::new(take(body)?).ok_or(RecordError::Field)?,
    })
}

fn read_cleanup(
    body: &mut Reader<'_>,
    relay: RelayIdentity,
    share: ShareId,
) -> Result<StopCleanup, RecordError> {
    match take::<1>(body)?[0] {
        CLEANUP_NONE => Ok(StopCleanup::None),
        CLEANUP_RESERVATION => Ok(StopCleanup::Reservation(read_operation(
            body, relay, share,
        )?)),
        CLEANUP_OWNER => Ok(StopCleanup::Owner(read_connection(body)?)),
        _ => Err(RecordError::Field),
    }
}

fn put_registration(out: &mut Writer<MAX_RECORD_BYTES>, registration: &Registration) {
    out.put(registration.share_instance().as_bytes())
        .put(registration.route().pk_hash().as_bytes())
        .put(registration.descriptor_digest().as_bytes())
        .put(registration.resume_token_hash().as_bytes());
}

fn put_connection(out: &mut Writer<MAX_RECORD_BYTES>, connection: ConnectionRef) {
    out.put(&connection.encode());
}

fn put_ownership(out: &mut Writer<MAX_RECORD_BYTES>, epoch: OwnershipEpoch, owner: ConnectionRef) {
    out.put(&epoch.get().get().to_be_bytes());
    put_connection(out, owner);
}

fn put_operation(out: &mut Writer<MAX_RECORD_BYTES>, id: OperationId) {
    out.put(id.incarnation.as_bytes());
    put_connection(out, id.connection);
    out.put(&id.sequence.get().to_be_bytes());
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

fn read_route(
    body: &mut Reader<'_>,
    relay: RelayIdentity,
    share_id: [u8; 12],
) -> Result<PublishedRoute, RecordError> {
    Ok(PublishedRoute {
        relay,
        registration: read_registration(body, share_id)?,
        attribution: crate::access::Attribution::decode(take(body)?).ok_or(RecordError::Field)?,
    })
}

fn read_connection(body: &mut Reader<'_>) -> Result<ConnectionRef, RecordError> {
    ConnectionRef::decode(take(body)?).ok_or(RecordError::Field)
}

fn read_operation(
    body: &mut Reader<'_>,
    relay: RelayIdentity,
    share: ShareId,
) -> Result<OperationId, RecordError> {
    let incarnation = Incarnation::new(take(body)?).ok_or(RecordError::Field)?;
    let connection = read_connection(body)?;
    Ok(OperationId {
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
