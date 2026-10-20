//! Structured decisions; hosts choose their own logging or tracing backend.

use crate::{
    clock::UnixMillis,
    identifiers::{RelayIdentity, ShareId, StopId},
    sessions::{Retirement, SessionKey},
};

use super::{
    Action, Cancellation, ConnectionRef, Event, Incarnation, OperationId, OwnershipId, Refusal,
    ShareLifecycle, State, StopCleanup, StopResult, StopStep,
};

/// Lifecycle phases shared by both platform adapters.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Phase {
    /// No pending or published registration.
    Vacant,
    /// A handshake owns the route while authenticating.
    Reserved,
    /// Authenticated ownership is waiting for its final durable confirmation.
    Activating,
    /// A sender connection owns the visible route.
    Published,
    /// No connection serves the published route until its sender resumes or
    /// the grace deadline lapses it.
    Grace,
    /// An irreversible STOP still needs durable recording or exact cleanup.
    Stopping,
    /// The share instance was permanently stopped.
    Stopped,
}

/// Milestone or control-flow input being observed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ObservedEvent {
    /// The actor was constructed from a stored record, or its absence.
    Restore,
    /// Restore decided the surviving session snapshot at this host index.
    RestoreSession(usize),
    /// A handshake requested a route reservation.
    Reserve,
    /// A handshake supplied its authenticated upload.
    Publish,
    /// The exact ownership record was durably confirmed.
    OwnershipCommitted,
    /// The attempt lost its owner or failed.
    Cancel(Cancellation),
    /// A handshake asked to resume the route.
    BeginResume,
    /// A handshake supplied its authenticated resume.
    Resume,
    /// The route's owner connection ended without STOP.
    OwnerDisconnected,
    /// An authenticated sender requested permanent retirement.
    Stop(StopId),
    /// Host evidence about one phase of irreversible retirement.
    StopFinished {
        /// Phase the callback names, including out-of-order callbacks.
        step: StopStep,
        /// Whether that phase is confirmed complete.
        result: StopResult,
    },
    /// A receiver checked readiness.
    CheckJoin,
    /// A connection-bound receiver request was admitted or refused.
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
    /// Requested the final durability barrier for publication or resume.
    CommitOwnership {
        /// Exact former owner whose authority was revoked immediately.
        replaced: Option<ConnectionRef>,
    },
    /// Confirmed durable ownership and permitted REGISTERED and JOIN.
    Registered,
    /// Bound a resume to the current ownership.
    ResumeAdmitted,
    /// Started the grace of a route whose owner is gone.
    Detached,
    /// Requested or retried one STOP phase.
    Stop {
        /// Requested persistence or cleanup phase.
        step: StopStep,
        /// Exact old participants, including after a restore.
        cleanup: StopCleanup,
    },
    /// Permitted STOPPED after durable revocation and unsubmitted-queue discard.
    Stopped,
    /// Removed a grace route whose sender never resumed.
    Lapsed,
    /// Allowed receiver admission.
    JoinReady,
    /// Created an exact receiver admission and permit generation.
    Joined(SessionKey),
    /// Reinstalled a surviving admission, permits included.
    SessionRestored(SessionKey),
    /// Retired a surviving snapshot instead of reinstalling it.
    SessionRetired(Retirement),
    /// Released an unpublished attempt.
    Released,
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
    /// The route's ownership after the decision: its live owner, or the one a
    /// grace route lost. Its epoch stays unchanged across restores.
    pub ownership: Option<OwnershipId>,
    /// Attempt being acted on, including stale callback identities, a
    /// pending ownership commit and a re-adopted owner.
    pub operation: Option<OperationId>,
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
pub use crate::observation::Observer;

impl<O: Observer> ShareLifecycle<O> {
    pub(super) fn observe(
        &mut self,
        at: UnixMillis,
        event: ObservedEvent,
        operation: Option<OperationId>,
        connection: Option<ConnectionRef>,
        before: Phase,
        decision: Decision,
    ) {
        self.observer.observe(
            Observation {
                at,
                relay: self.relay,
                share: self.share,
                incarnation: self.incarnation,
                ownership: match self.state {
                    State::Activating(commit) => Some(commit.route.ownership()),
                    State::Published(published) => Some(published.owner),
                    State::Grace(route) => Some(route.lost()),
                    _ => None,
                },
                operation,
                connection,
                event,
                before,
                after: self.phase(),
                decision,
            }
            .into(),
        );
    }
}

pub(super) const fn event_context(
    event: &Event<'_>,
) -> (ObservedEvent, Option<OperationId>, Option<ConnectionRef>) {
    match event {
        Event::Terminate { stop_id, .. } => (ObservedEvent::Stop(*stop_id), None, None),
        Event::Reserve { connection, .. } => (ObservedEvent::Reserve, None, Some(*connection)),
        Event::Publish(publication) => (
            ObservedEvent::Publish,
            Some(publication.id()),
            Some(publication.id().connection()),
        ),
        Event::OwnershipCommitted(id) => (
            ObservedEvent::OwnershipCommitted,
            Some(*id),
            Some(id.connection()),
        ),
        Event::Cancel { id, reason } => (
            ObservedEvent::Cancel(*reason),
            Some(*id),
            Some(id.connection()),
        ),
        Event::BeginResume { connection, .. } => {
            (ObservedEvent::BeginResume, None, Some(*connection))
        }
        Event::Resume(resumption) => (
            ObservedEvent::Resume,
            Some(resumption.id()),
            Some(resumption.id().connection()),
        ),
        Event::OwnerDisconnected(owner) => (ObservedEvent::OwnerDisconnected, None, Some(*owner)),
        Event::Stop {
            connection,
            authority,
        } => (
            ObservedEvent::Stop(authority.init().stop_id()),
            None,
            Some(*connection),
        ),
        Event::StopFinished { work, result } => (
            ObservedEvent::StopFinished {
                step: work.step(),
                result: *result,
            },
            Some(work.pending().id()),
            Some(work.pending().id().connection()),
        ),
        Event::CheckJoin(_) => (ObservedEvent::CheckJoin, None, None),
        Event::Join { request, .. } => (ObservedEvent::Join, None, Some(request.connection())),
        Event::Tick => (ObservedEvent::Tick, None, None),
    }
}

pub(super) const fn decision(action: &Action<'_>) -> Decision {
    match action {
        Action::Reserved(_) => Decision::Reserved,
        Action::Register(request) => Decision::CommitOwnership {
            replaced: request.ownership.replaced(),
        },
        Action::Registered(_) => Decision::Registered,
        Action::CommitOwnership(commit) => Decision::CommitOwnership {
            replaced: commit.replaced(),
        },
        Action::ResumeAdmitted(_) => Decision::ResumeAdmitted,
        Action::Detached(_) => Decision::Detached,
        Action::Stop(work) => Decision::Stop {
            step: work.step(),
            cleanup: work.pending().cleanup(),
        },
        Action::Stopped(_) => Decision::Stopped,
        Action::JoinReady(_) => Decision::JoinReady,
        Action::Joined(joined) => Decision::Joined(joined.session.key()),
        Action::Released(_) => Decision::Released,
        Action::Refused(reason) => Decision::Refused(*reason),
        Action::None => Decision::None,
    }
}
