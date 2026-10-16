//! Identities and capabilities that survive asynchronous host work.

use core::num::NonZeroU64;

use crate::{
    clock::UnixMillis,
    descriptor_upload::AuthorizedDescriptor,
    identifiers::{RelayIdentity, ShareId},
    registration::{RegisterInit, Registered, ResumeRegistration},
};

use super::{OwnedRoute, PublishedRoute, Record, UnresolvedPublication};

/// One lifetime of a host connection slot. Reusing a slot requires a new generation.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ConnectionRef {
    /// Host-local socket slot or identifier.
    pub id: NonZeroU64,
    /// Never reused for this slot while an old callback can still arrive.
    pub generation: NonZeroU64,
}

/// A host-chosen, nonzero incarnation of a single-share lifecycle.
///
/// Every restore needs a fresh value: a random 128-bit value or a durably
/// allocated counter keeps callbacks from before the restore from naming new
/// work. Stored operations keep the incarnation that minted them.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Incarnation([u8; 16]);

impl Incarnation {
    /// Rejects the reserved all-zero value.
    #[must_use]
    pub fn new(bytes: [u8; 16]) -> Option<Self> {
        (bytes != [0; 16]).then_some(Self(bytes))
    }

    /// Returns the encoded incarnation.
    #[must_use]
    pub const fn as_bytes(&self) -> &[u8; 16] {
        &self.0
    }
}

/// Correlation identity minted by the lifecycle, never by a callback.
///
/// Registrations, resumes, stops and receiver admissions share this identity space.
/// The relay, share and incarnation fence independent actors; the connection
/// generation and checked sequence fence retries within one actor.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct OperationId {
    pub(super) relay: RelayIdentity,
    pub(super) share: ShareId,
    pub(super) incarnation: Incarnation,
    pub(super) connection: ConnectionRef,
    pub(super) sequence: NonZeroU64,
}

impl OperationId {
    // Receiver admissions share the owner's scope, but never its participant or
    // sequence. Keeping that common scope once makes queued permits compact.
    pub(crate) const fn for_session(self, connection: ConnectionRef, sequence: NonZeroU64) -> Self {
        Self {
            connection,
            sequence,
            ..self
        }
    }

    /// Returns the relay whose lifecycle this operation belongs to.
    #[must_use]
    pub const fn relay(self) -> RelayIdentity {
        self.relay
    }

    /// Returns the share this operation belongs to.
    #[must_use]
    pub const fn share(self) -> ShareId {
        self.share
    }

    /// Returns the engine incarnation that minted this operation.
    #[must_use]
    pub const fn incarnation(self) -> Incarnation {
        self.incarnation
    }

    /// Returns the exact connection lifetime that owns this attempt.
    #[must_use]
    pub const fn connection(self) -> ConnectionRef {
        self.connection
    }

    /// Returns the never-wrapping sequence within this engine incarnation.
    #[must_use]
    pub const fn sequence(self) -> NonZeroU64 {
        self.sequence
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) struct Pending {
    pub id: OperationId,
    pub init: RegisterInit,
    pub deadline: UnixMillis,
}

impl Pending {
    /// Drops the handshake deadline: once fenced, only reconciliation ends
    /// the attempt, never a timeout.
    pub const fn unresolved(self) -> UnresolvedPublication {
        UnresolvedPublication {
            id: self.id,
            registration: *self.init.registration(),
        }
    }
}

/// Admission of one fresh handshake. Moving it into the handshake binds the
/// eventual upload to this exact reservation, even if a newer one replaces it.
#[derive(Debug, Eq, PartialEq)]
pub struct Reservation(pub(super) Pending);

impl Reservation {
    /// Returns the identity used to cancel failed or disconnected handshakes.
    #[must_use]
    pub const fn id(&self) -> OperationId {
        self.0.id
    }

    /// Returns the registration the share reserved.
    #[must_use]
    pub const fn init(&self) -> &RegisterInit {
        &self.0.init
    }

    /// Returns the original handshake deadline, including host publication work.
    #[must_use]
    pub const fn deadline(&self) -> UnixMillis {
        self.0.deadline
    }
}

/// An authenticated upload bound to the reservation its handshake consumed.
#[derive(Debug, Eq, PartialEq)]
pub struct Publication<'f> {
    pub(super) id: OperationId,
    pub(super) deadline: UnixMillis,
    pub(super) descriptor: AuthorizedDescriptor<'f>,
}

impl<'f> Publication<'f> {
    pub(crate) const fn new(
        id: OperationId,
        deadline: UnixMillis,
        descriptor: AuthorizedDescriptor<'f>,
    ) -> Self {
        Self {
            id,
            deadline,
            descriptor,
        }
    }

    /// Returns the attempt whose publication was authenticated.
    #[must_use]
    pub const fn id(&self) -> OperationId {
        self.id
    }

    /// Returns the authenticated registration and opaque descriptor bytes.
    #[must_use]
    pub const fn descriptor(&self) -> &AuthorizedDescriptor<'f> {
        &self.descriptor
    }
}

/// Host transaction requested after authentication, before receiver visibility.
///
/// Prepare connection resources and durably store [`Self::record`] with these
/// exact opaque descriptor bytes, conditional on the share having no record.
/// Do not expose the route or send REGISTERED. Report the outcome through
/// `Event::PublicationFinished`. If work crosses an await, copy only the
/// bounded descriptor into host storage.
#[derive(Debug, Eq, PartialEq)]
pub struct PublishRequest<'f> {
    /// All resource and storage changes must be conditional on this identity.
    pub id: OperationId,
    /// Metadata authenticated by the sender.
    pub init: RegisterInit,
    /// Opaque, authenticated upload; never a parsed end-to-end object.
    pub descriptor: &'f [u8],
    /// Host work cannot extend the handshake budget.
    pub deadline: UnixMillis,
}

impl PublishRequest<'_> {
    /// The record the transaction stores. Until REGISTERED replaces it, a
    /// restart restores this attempt fenced, so a commit the lifecycle never
    /// acknowledged cannot become visible.
    #[must_use]
    pub const fn record(&self) -> Record {
        Record::Unresolved(UnresolvedPublication {
            id: self.id,
            registration: *self.init.registration(),
        })
    }
}

/// Admission of one resume handshake, bound to the route ownership observed
/// before its challenge.
///
/// It holds no route capacity, so a failed or abandoned handshake needs no
/// lifecycle event. Its commit fails as stale once a publication, another
/// resume or a restore replaced that ownership; the owner merely
/// disconnecting does not, since no new owner exists to protect.
#[derive(Debug, Eq, PartialEq)]
pub struct ResumeAttempt {
    pub(super) id: OperationId,
    pub(super) init: RegisterInit,
    pub(super) deadline: UnixMillis,
    // The observed ownership's sequence, or `None` when no route existed, so
    // an authenticated sender can learn `NotFound` and publish again.
    pub(super) ownership: Option<NonZeroU64>,
}

impl ResumeAttempt {
    /// Returns the identity the resuming connection owns the route under.
    #[must_use]
    pub const fn id(&self) -> OperationId {
        self.id
    }

    /// Returns the registration the resume names.
    #[must_use]
    pub const fn init(&self) -> &RegisterInit {
        &self.init
    }

    /// Returns the original handshake deadline, which the commit must meet.
    #[must_use]
    pub const fn deadline(&self) -> UnixMillis {
        self.deadline
    }
}

/// An authenticated resume bound to the attempt its handshake consumed.
#[derive(Debug, Eq, PartialEq)]
pub struct Resumption {
    pub(super) attempt: ResumeAttempt,
}

impl Resumption {
    /// The handshake authenticates exactly the attempt's registration, so
    /// the spent authority would only duplicate it.
    #[expect(
        clippy::needless_pass_by_value,
        reason = "consuming the authority makes authentication the only way to mint a resumption"
    )]
    pub(crate) fn new(attempt: ResumeAttempt, authority: ResumeRegistration) -> Self {
        debug_assert_eq!(authority.init(), &attempt.init);
        Self { attempt }
    }

    /// Returns the attempt whose resume was authenticated.
    #[must_use]
    pub const fn id(&self) -> OperationId {
        self.attempt.id
    }

    /// Returns the authenticated registration.
    #[must_use]
    pub const fn init(&self) -> &RegisterInit {
        &self.attempt.init
    }
}

/// A publication or resume whose sender connection owns the published route.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PublishedRegistration {
    /// The owning sender's operation: resource ownership and callback
    /// correlation. A restore re-adopting the owner mints a new one.
    pub id: OperationId,
    /// The durable route receivers are admitted to.
    pub route: PublishedRoute,
}

impl PublishedRegistration {
    /// Only `Action::Registered` and `Action::Resumed` authorize this reply.
    #[must_use]
    pub const fn acknowledgement(self) -> Registered {
        Registered::new(&self.route.registration)
    }

    /// The record naming this owner, stored before its acknowledgement.
    #[must_use]
    pub const fn record(self) -> Record {
        Record::Published(OwnedRoute {
            route: self.route,
            owner: self.id.connection,
        })
    }
}

/// A committed resume: the route's new owner, and the owner it replaced.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Resumed {
    /// The resuming connection now owns the route.
    pub registration: PublishedRegistration,
    /// The owner still connected when the resume committed. Close exactly
    /// this lifetime and its relay sessions; later events naming it are stale.
    pub replaced: Option<ConnectionRef>,
}
