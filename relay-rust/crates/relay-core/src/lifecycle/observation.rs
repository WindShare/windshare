//! Structured decisions; hosts choose their own logging or tracing backend.

use crate::{
    clock::UnixMillis,
    identifiers::{RelayIdentity, ShareId},
};

use super::{Cancellation, ConnectionRef, Incarnation, PublicationResult, Refusal, RegistrationId};

/// Lifecycle phases shared by both platform adapters.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Phase {
    /// No pending or published registration.
    Vacant,
    /// A handshake owns the route while authenticating.
    Reserved,
    /// Authentication finished; persistence/resources are not yet confirmed.
    Publishing,
    /// An abandoned or uncertain publication must be reconciled.
    Fenced,
    /// The host confirmed publication and the route is visible.
    Published,
    /// A restored route is published, but no sender connection serves it yet.
    Detached,
    /// The share instance was permanently stopped.
    Stopped,
}

/// Milestone or control-flow input being observed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ObservedEvent {
    /// The actor was constructed from a stored record, or its absence.
    Restore,
    /// A handshake requested a route reservation.
    Reserve,
    /// A handshake supplied its authenticated upload.
    Publish,
    /// Host publication or reconciliation result.
    PublicationFinished(PublicationResult),
    /// The attempt lost its owner or failed.
    Cancel(Cancellation),
    /// A receiver checked readiness.
    Join,
    /// A timer event without an expired operation.
    Tick,
    /// An event or timer discovered an expired operation.
    Deadline,
}

/// Decision made for the observed event.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Decision {
    /// Reserved route capacity.
    Reserved,
    /// Requested host publication.
    Publish,
    /// Acknowledged publication.
    Registered,
    /// Allowed receiver admission.
    JoinReady,
    /// Released an unpublished attempt.
    Released,
    /// Kept a possibly committed transaction hidden.
    Reconcile,
    /// Rejected or ignored an input.
    Refused(Refusal),
    /// No lifecycle work was needed.
    None,
}

/// One decision, without descriptor bytes or resume credentials.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Observation {
    /// Time supplied with the input.
    pub at: UnixMillis,
    /// Hosting relay.
    pub relay: RelayIdentity,
    /// Selected share actor.
    pub share: ShareId,
    /// Actor lifetime.
    pub incarnation: Incarnation,
    /// Attempt being acted on, including stale callback identities and a
    /// restored unresolved operation.
    pub operation: Option<RegistrationId>,
    /// Requesting connection, present even before a reservation exists.
    pub connection: Option<ConnectionRef>,
    /// Cause of the decision.
    pub event: ObservedEvent,
    /// State before the decision; the restored phase itself for `Restore`.
    pub before: Phase,
    /// State after the decision.
    pub after: Phase,
    /// Result or refusal reason.
    pub decision: Decision,
}

/// Static-dispatch boundary for platform trace sinks. Calls are synchronous;
/// implementations must not re-enter the lifecycle or retain payload buffers.
pub trait Observer {
    /// Records one structured decision.
    fn observe(&mut self, observation: Observation);
}

impl Observer for () {
    fn observe(&mut self, _: Observation) {}
}
