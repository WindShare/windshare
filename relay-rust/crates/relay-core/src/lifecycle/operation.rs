//! Identities and capabilities that survive asynchronous host work.

use core::num::NonZeroU64;

use crate::{
    clock::UnixMillis,
    descriptor_upload::AuthorizedDescriptor,
    identifiers::{RelayIdentity, ShareId},
    registration::RegisterInit,
};

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
/// Hosts retain this value while restoring the same engine state, and choose a
/// fresh value when constructing a replacement. A random 128-bit value or a
/// durably allocated counter keeps pre-restart callbacks from naming new work.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Incarnation([u8; 16]);

impl Incarnation {
    /// Rejects the reserved all-zero value.
    #[must_use]
    pub fn new(bytes: [u8; 16]) -> Option<Self> {
        (bytes != [0; 16]).then_some(Self(bytes))
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
/// Prepare connection resources and durably store the registration plus these
/// exact opaque descriptor bytes, all under `id`. Do not expose the route or
/// send REGISTERED. Report the outcome through `Event::PublicationFinished`.
/// If work crosses an await, copy only the bounded descriptor into host storage.
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

/// A publication whose host transaction completed while the sender still owned it.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PublishedRegistration {
    /// Resource ownership and callback correlation.
    pub id: RegistrationId,
    /// The committed registration, including its resume commitment.
    pub init: RegisterInit,
}

impl PublishedRegistration {
    /// The only lifecycle action that authorizes this reply is `Action::Registered`.
    #[must_use]
    pub const fn acknowledgement(self) -> crate::registration::Registered {
        crate::registration::Registered::new(&self.init)
    }
}
