//! Exact identities carried across queues and delayed host callbacks.

use core::num::NonZeroU64;

use crate::{
    WIRE_VERSION,
    identifiers::{RelayIdentity, RelaySessionId, ShareId, ShareInstance},
    lifecycle::{
        ConnectionRef, Incarnation, Observer as LifecycleObserver, OperationId, OwnershipId,
        ShareLifecycle,
    },
};

use super::{RetirementReason, Session};

/// One receiver admission, including the sender ownership it joined.
///
/// Nothing here names the actor incarnation serving it, so rebuilding the
/// actor over the same sockets keeps the key, and every permit carrying it.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SessionKey {
    pub(super) owner: OwnershipId,
    // A restored actor admits under its own fresh incarnation, so its new
    // admissions never collide with surviving ones without a durable counter.
    pub(super) admitted_by: Incarnation,
    pub(super) receiver: ConnectionRef,
    pub(super) admission_sequence: NonZeroU64,
    pub(super) wire_id: RelaySessionId,
    pub(super) instance: ShareInstance,
}

impl SessionKey {
    /// Correlates the admission and its non-reusable permit generation.
    #[must_use]
    pub const fn admission(self) -> OperationId {
        OperationId::admission(
            self.owner,
            self.admitted_by,
            self.receiver,
            self.admission_sequence,
        )
    }

    /// The exact sender ownership, not merely its socket slot.
    #[must_use]
    pub const fn owner(self) -> OwnershipId {
        self.owner
    }

    /// The routing ID carried by WS2O and WS2M.
    #[must_use]
    pub const fn wire_id(self) -> RelaySessionId {
        self.wire_id
    }

    /// Hosting relay.
    #[must_use]
    pub const fn relay(self) -> RelayIdentity {
        self.owner.relay()
    }

    /// Share routing identity.
    #[must_use]
    pub const fn share(self) -> ShareId {
        self.owner.share()
    }

    /// Published content instance.
    #[must_use]
    pub const fn instance(self) -> ShareInstance {
        self.instance
    }

    /// Exact sending connection lifetime.
    #[must_use]
    pub const fn sender(self) -> ConnectionRef {
        self.owner.connection()
    }

    /// Exact receiving connection lifetime.
    #[must_use]
    pub const fn receiver(self) -> ConnectionRef {
        self.receiver
    }

    pub(super) fn direction(self, source: ConnectionRef) -> Option<Direction> {
        if source == self.sender() {
            Some(Direction::ToReceiver)
        } else if source == self.receiver() {
            Some(Direction::ToSender)
        } else {
            None
        }
    }

    /// Whether both keys claim one routing ID or one issued admission, either
    /// of which an actor hands out once.
    pub(crate) fn collides(self, other: Self) -> bool {
        self.wire_id == other.wire_id
            || (self.admitted_by == other.admitted_by
                && self.admission_sequence == other.admission_sequence)
    }

    pub(crate) const fn admitted_by(self) -> Incarnation {
        self.admitted_by
    }
}

/// Direction comes from the exact connection, never from opaque payload bytes.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Direction {
    /// Receiver to the registered sender.
    ToSender,
    /// Registered sender to this receiver.
    ToReceiver,
}

/// A queued routing capability. This is not evidence that it is still valid.
///
/// Hosts must call `Session::check_forward` immediately before submission and
/// serialize that check/submission with session and ownership transitions.
/// Neither this permit nor a successful socket write grants or refunds credit.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ForwardPermit {
    pub(super) session: SessionKey,
    pub(super) direction: Direction,
}

impl ForwardPermit {
    /// Includes relay, share, session, ownership epoch, admission and both
    /// connection generations.
    #[must_use]
    pub const fn session(self) -> SessionKey {
        self.session
    }

    /// Protocol version this authorization was issued for.
    #[must_use]
    pub const fn protocol_version(self) -> u8 {
        WIRE_VERSION
    }

    /// Direction authorized for this session.
    #[must_use]
    pub const fn direction(self) -> Direction {
        self.direction
    }

    /// Resolve this exact lifetime, never its slot alone.
    #[must_use]
    pub const fn source(self) -> ConnectionRef {
        match self.direction {
            Direction::ToSender => self.session.receiver(),
            Direction::ToReceiver => self.session.sender(),
        }
    }

    /// Resolve this exact lifetime, never its slot alone.
    #[must_use]
    pub const fn destination(self) -> ConnectionRef {
        match self.direction {
            Direction::ToSender => self.session.sender(),
            Direction::ToReceiver => self.session.receiver(),
        }
    }
}

/// A send-time check that borrows both authorities through synchronous submission.
///
/// Do not retain it across an await. A queued or delayed submission must check
/// again; adapters with parallel writers synchronize this borrow with revocation.
/// The frame submitted must have the permit's wire session ID and version.
#[derive(Debug)]
pub struct ValidatedForward<'a, O> {
    pub(super) permit: ForwardPermit,
    pub(super) _session: &'a Session,
    pub(super) _lifecycle: &'a ShareLifecycle<O>,
}

impl<O: LifecycleObserver> ValidatedForward<'_, O> {
    /// Exact route to submit while the authorities remain borrowed.
    #[must_use]
    pub const fn permit(&self) -> ForwardPermit {
        self.permit
    }
}

/// Why a queued frame can no longer be submitted.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ForwardRefusal {
    /// Another relay/share's authority was supplied.
    ForeignLifecycle,
    /// The queued permit names a different admission.
    StaleSession,
    /// Admission expired, the session ended, or its owner was revoked.
    Retired(RetirementReason),
}

/// Lifecycle-issued instruction to retire only sessions of one old ownership.
///
/// Forward checks consult live lifecycle state too, so delaying this cleanup
/// cannot leave old permits usable while STOP persistence is in flight.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Revocation {
    pub(crate) owner: OwnershipId,
    pub(crate) reason: RetirementReason,
}

impl Revocation {
    /// Exact old ownership; a later resume on the same socket is different.
    #[must_use]
    pub const fn owner(self) -> OwnershipId {
        self.owner
    }

    /// Why that ownership lost all forwarding authority.
    #[must_use]
    pub const fn reason(self) -> RetirementReason {
        self.reason
    }
}
