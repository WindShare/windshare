//! Durable share records: the only facts a host persists, and the only input
//! that constructs a share lifecycle.
//!
//! Reservations and resume attempts never reach storage. A publication
//! transaction stores its unresolved record before it can commit, and
//! REGISTERED first replaces that record with the published route and its
//! ownership; every RESUME rewrites that ownership before its acknowledgement.
//! A crash at any point therefore restores a fenced attempt that still needs
//! reconciliation, or a route whose sender may already hold REGISTERED, never
//! a revived attempt the lifecycle fenced. Restoring the stored ownership over
//! its surviving owner is no ownership change, so its sessions stay valid.
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
const FORMAT: u8 = 4;

const UNRESOLVED: u8 = 1;
const PUBLISHED: u8 = 2;
const GRACE: u8 = 3;
const STOPPED: u8 = 4;
const STOPPING: u8 = 5;

const CLEANUP_NONE: u8 = 0;
const CLEANUP_RESERVATION: u8 = 1;
const CLEANUP_PUBLICATION: u8 = 2;
const CLEANUP_OWNER: u8 = 3;

// Format and kind, then the relay identity and share ID every record names.
const HEADER_BYTES: usize = 1 + 1 + 32 + 12;
// Instance, key hash, descriptor digest and resume commitment: the WS2R body
// after its share ID, which the header already holds.
const REGISTRATION_BYTES: usize = 16 + 16 + 32 + 32;
// Connection slot and generation.
const CONNECTION_BYTES: usize = ConnectionRef::ENCODED_BYTES;
// Incarnation and connection, then the attempt sequence.
const OPERATION_BYTES: usize = 16 + CONNECTION_BYTES + 8;
// Epoch, then the owner connection.
const OWNERSHIP_BYTES: usize = 8 + CONNECTION_BYTES;
// A grace deadline beside the lost ownership keeps grace records within the
// unresolved bound that hosts size storage from.
const _: () = assert!(8 + OWNERSHIP_BYTES <= OPERATION_BYTES);

// Instance, key hash and stop ID.
const STOP_FACT_BYTES: usize = 16 + 16 + 16;
// The cleanup tag and at most one publication operation keep a stopping
// record smaller than an unresolved publication.
const _: () = assert!(STOP_FACT_BYTES + 1 + OPERATION_BYTES <= REGISTRATION_BYTES);

/// Largest encoded record, an unresolved publication; hosts may size storage
/// columns from it.
pub const MAX_RECORD_BYTES: usize = HEADER_BYTES + REGISTRATION_BYTES + OPERATION_BYTES;

/// A publication whose transaction may have committed.
///
/// Neither a timeout nor a restart may forget it: only reconciliation of this
/// exact operation, ending in `PublicationResult::Aborted`, removes it.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct UnresolvedPublication {
    pub(super) id: OperationId,
    pub(super) registration: Registration,
}

impl UnresolvedPublication {
    /// Returns the operation whose resources and record need reconciliation.
    #[must_use]
    pub const fn id(&self) -> OperationId {
        self.id
    }

    /// Returns the registration the transaction may have stored.
    #[must_use]
    pub const fn registration(&self) -> &Registration {
        &self.registration
    }
}

/// A registration the lifecycle acknowledged with REGISTERED. Every owner
/// that resumes it serves these same facts.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PublishedRoute {
    pub(super) relay: RelayIdentity,
    pub(super) registration: Registration,
}

impl PublishedRoute {
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

/// A published route and the ownership that served it when last written.
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

    /// Returns the ownership that served the route.
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
    /// Stored by the publication transaction, from `PublishRequest::record`.
    Unresolved(UnresolvedPublication),
    /// Replaces the unresolved record before REGISTERED, and the stored record
    /// before every RESUME acknowledgement, from `PublishedRegistration::record`.
    Published(OwnedRoute),
    /// Replaces the owned record once its owner is gone, from
    /// `GraceRoute::record`; removed when the route lapses.
    Grace(GraceRoute),
    /// Stored before STOP cleanup, retaining its exact participants across
    /// restart. It may become Stopped, never a live route or an absent record.
    Stopping(PendingStop),
    /// Stored after STOP cleanup and before STOPPED; never removed.
    Stopped(StoppedShare),
}

impl Record {
    /// Returns the relay whose storage owns this record.
    #[must_use]
    pub const fn relay(&self) -> RelayIdentity {
        match self {
            Self::Unresolved(unresolved) => unresolved.id.relay,
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
            Self::Unresolved(unresolved) => unresolved.registration.route().share_id(),
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
            Self::Unresolved(_) => UNRESOLVED,
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
            Self::Unresolved(unresolved) => {
                put_registration(&mut out, &unresolved.registration);
                put_operation(&mut out, unresolved.id);
            }
            Self::Published(owned) => {
                put_registration(&mut out, &owned.route.registration);
                put_ownership(&mut out, owned.epoch, owned.owner);
            }
            Self::Grace(grace) => {
                put_registration(&mut out, &grace.route.registration);
                out.put(&grace.deadline.0.to_be_bytes());
                put_ownership(&mut out, grace.epoch, grace.owner);
            }
            Self::Stopping(pending) => {
                put_stopped(&mut out, pending.stopped());
                put_operation(&mut out, pending.id);
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
            UNRESOLVED => {
                let registration = read_registration(&mut body, share_id)?;
                Self::Unresolved(UnresolvedPublication {
                    id: read_operation(&mut body, relay, registration.route().share_id())?,
                    registration,
                })
            }
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
                Self::Stopping(PendingStop::new(
                    read_operation(&mut body, relay, share)?,
                    stopped,
                    read_cleanup(&mut body, relay, share)?,
                ))
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
        StopCleanup::Reservation(id) | StopCleanup::Publication(id) => {
            let kind = if matches!(cleanup, StopCleanup::Reservation(_)) {
                CLEANUP_RESERVATION
            } else {
                CLEANUP_PUBLICATION
            };
            out.put(&[kind]);
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
        CLEANUP_PUBLICATION => Ok(StopCleanup::Publication(read_operation(
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
