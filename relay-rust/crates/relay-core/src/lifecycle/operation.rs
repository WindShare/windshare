//! Identities and capabilities that survive asynchronous host work.

use core::num::NonZeroU64;

use crate::{
    clock::UnixMillis,
    descriptor_upload::AuthorizedDescriptor,
    identifiers::{RelayIdentity, ShareId},
    registration::{RegisterInit, Registered},
};

use super::{PublishedRoute, Record, UnresolvedPublication};

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
/// The relay, share and incarnation fence independent actors; the connection
/// generation and checked sequence fence retries within one actor.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RegistrationId {
    pub(super) relay: RelayIdentity,
    pub(super) share: ShareId,
    pub(super) incarnation: Incarnation,
    pub(super) connection: ConnectionRef,
    pub(super) sequence: NonZeroU64,
}

impl RegistrationId {
    /// Returns the relay whose proof and publication this operation belongs to.
    #[must_use]
    pub const fn relay(self) -> RelayIdentity {
        self.relay
    }

    /// Returns the share this operation may publish.
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
    pub id: RegistrationId,
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
    pub const fn id(&self) -> RegistrationId {
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
    pub(super) id: RegistrationId,
    pub(super) deadline: UnixMillis,
    pub(super) descriptor: AuthorizedDescriptor<'f>,
}

impl<'f> Publication<'f> {
    pub(crate) const fn new(
        id: RegistrationId,
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
    pub const fn id(&self) -> RegistrationId {
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
    pub id: RegistrationId,
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

/// A publication whose host transaction completed while the sender still owned it.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PublishedRegistration {
    /// The owning sender's operation: resource ownership and callback correlation.
    pub id: RegistrationId,
    /// The durable route receivers are admitted to.
    pub route: PublishedRoute,
}

impl PublishedRegistration {
    /// The only lifecycle action that authorizes this reply is `Action::Registered`.
    #[must_use]
    pub const fn acknowledgement(self) -> Registered {
        Registered::new(&self.route.registration)
    }

    /// The record that replaces this publication's unresolved record before
    /// REGISTERED is sent.
    #[must_use]
    pub const fn record(self) -> Record {
        Record::Published(self.route)
    }
}
