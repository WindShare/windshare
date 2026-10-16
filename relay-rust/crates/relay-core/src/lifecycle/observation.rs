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
    /// A sender connection owns the visible route.
    Published,
    /// No connection serves the published route until its sender resumes or
    /// the grace deadline lapses it.
    Grace,
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
    /// A handshake asked to resume the route.
    BeginResume,
    /// A handshake supplied its authenticated resume.
    Resume,
    /// The route's owner connection ended without STOP.
    OwnerDisconnected,
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
    /// Bound a resume to the current ownership.
    ResumeAdmitted,
    /// Transferred ownership to the resuming connection.
    Resumed,
    /// Started the grace of a route whose owner is gone.
    Detached,
    /// Removed a grace route whose sender never resumed.
    Lapsed,
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
    /// Attempt being acted on, including stale callback identities, a
    /// restored unresolved operation and a re-adopted owner.
    pub operation: Option<RegistrationId>,
    /// Requesting connection, present even before a reservation exists, or
    /// the owner a disconnect or restored record names.
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
