//! Structured milestones without introducing a logging runtime into the kernel.

use crate::clock::UnixMillis;

use super::{Action, Event, Phase, Refusal, RetirementReason, SessionKey};

/// Stable decision vocabulary shared by both hosts.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Decision {
    /// The first receiver frame advanced admission without renewing it.
    ReceiverObserved,
    /// Sender confirmation made the session active.
    Active,
    /// Forwarding was revoked before resource cleanup.
    Retired(RetirementReason),
    /// A former participant's late traffic was discarded.
    Discarded(RetirementReason),
    /// A foreign, stale or out-of-order input was refused.
    Refused(Refusal),
}

/// One milestone, with correlation identity and no opaque bytes.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Observation {
    /// Injected wall clock.
    pub at: UnixMillis,
    /// Current relay/share, wire ID, ownership and connection generations.
    pub session: SessionKey,
    /// Bounded input metadata, including the rejected wire ID, connection or
    /// callback's original session key. Keeping it avoids attributing stale
    /// work to the replacement session merely because its slot was reused.
    pub event: Event,
    /// State before processing.
    pub before: Phase,
    /// State after processing.
    pub after: Phase,
    /// Transition or reason.
    pub decision: Decision,
}

/// Hosts choose the synchronous tracing backend; steady forwarding emits no trace.
pub use crate::observation::Observer;

pub(super) const fn decision(action: Action, changed: bool) -> Option<Decision> {
    match action {
        Action::Forward(_) if changed => Some(Decision::ReceiverObserved),
        Action::Active if changed => Some(Decision::Active),
        Action::Discard(reason) => Some(Decision::Discarded(reason)),
        Action::Refused(reason) => Some(Decision::Refused(reason)),
        Action::Forward(_) | Action::Active | Action::None => None,
    }
}
