//! Receiver admission and revocable routing, independent of host storage or I/O.
//!
//! A lifecycle JOIN creates one move-only `Session`. Hosts reserve bounded
//! session capacity and allocate a nonzero wire ID before JOIN; IDs must never
//! be reused while either participant connection survives. Hosts store sessions
//! beside their platform's connection resources, retaining retired identities
//! for bounded late-frame handling. This module owns all session decisions.
//!
//! Hosts apply lifecycle `Effects::revocation` to the old owner's sessions,
//! retain each retirement until host cleanup finishes, and combine session
//! deadlines with handshake/lifecycle deadlines. STOP performs physical cleanup
//! only after its intent is durable. Send-time checks consult the live lifecycle
//! too: deferred cleanup cannot prolong forwarding authority.
//!
//! No payload, queue, timer, allocator or platform capacity lives here. The
//! session itself is not cloneable. Hosts whose sockets outlive the actor, such
//! as hibernated ones, record `Session::snapshot` beside them after every input;
//! a snapshot is inert data that only `ShareLifecycle::restore` validates and
//! turns back into the same session.

mod authority;
mod effect;
mod frames;
mod observation;
mod snapshot;

pub use authority::{
    Direction, ForwardPermit, ForwardRefusal, Revocation, SessionKey, ValidatedForward,
};
pub use effect::{
    Action, Effects, EndReason, Event, Refusal, Retirement, RetirementReason, SnapshotFault,
};
pub use frames::{MAX_OPAQUE_CIPHERTEXT_BYTES, OpaqueHeader, SessionAdmitted};
pub use observation::{Decision, Observation, Observer};
pub use snapshot::{EncodedSnapshot, MAX_SNAPSHOT_BYTES, SessionSnapshot};

use crate::{
    clock::UnixMillis,
    identifiers::RelaySessionId,
    lifecycle::{
        Observer as LifecycleObserver, OperationId, PublishedRegistration, ShareLifecycle,
    },
};

/// Admission phase visible in diagnostics.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Phase {
    /// Descriptor delivery and the receiver's first opaque frame are pending.
    AwaitingReceiver,
    /// A receiver frame arrived; the current sender must explicitly admit it.
    AwaitingSender,
    /// Browsing and P2P may leave the relay silent indefinitely.
    Active,
    /// This admission cannot become active or grant forwarding again.
    Retired,
}

/// The phases a session can still forward in, and the only ones a snapshot
/// records: a retired admission never becomes live again.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Stage {
    AwaitingReceiver { deadline: UnixMillis },
    AwaitingSender { deadline: UnixMillis },
    Active,
}

impl Stage {
    const fn phase(self) -> Phase {
        match self {
            Self::AwaitingReceiver { .. } => Phase::AwaitingReceiver,
            Self::AwaitingSender { .. } => Phase::AwaitingSender,
            Self::Active => Phase::Active,
        }
    }

    const fn deadline(self) -> Option<UnixMillis> {
        match self {
            Self::AwaitingReceiver { deadline } | Self::AwaitingSender { deadline } => {
                Some(deadline)
            }
            Self::Active => None,
        }
    }
}

#[derive(Debug, Eq, PartialEq)]
enum State {
    Live(Stage),
    Retired(RetirementReason),
}

/// Authority over one admission. Constructed only by the share lifecycle.
#[derive(Debug, Eq, PartialEq)]
pub struct Session {
    key: SessionKey,
    state: State,
}

impl Session {
    pub(crate) const fn new(
        admission: OperationId,
        published: PublishedRegistration,
        wire_id: RelaySessionId,
        deadline: UnixMillis,
    ) -> Self {
        Self {
            key: SessionKey {
                owner: published.owner,
                admitted_by: admission.incarnation(),
                receiver: admission.connection(),
                admission_sequence: admission.sequence(),
                wire_id,
                instance: published.route.registration().share_instance(),
            },
            state: State::Live(Stage::AwaitingReceiver { deadline }),
        }
    }

    /// Reinstalls a snapshot the restoring lifecycle has already validated.
    pub(crate) const fn restored(snapshot: SessionSnapshot) -> Self {
        Self {
            key: snapshot.key,
            state: State::Live(snapshot.stage),
        }
    }

    /// Exact identity to retain before asynchronous work.
    #[must_use]
    pub const fn key(&self) -> SessionKey {
        self.key
    }

    /// Current admission/retirement phase.
    #[must_use]
    pub const fn phase(&self) -> Phase {
        match self.state {
            State::Live(stage) => stage.phase(),
            State::Retired(_) => Phase::Retired,
        }
    }

    /// Original absolute admission deadline; no traffic can extend it.
    #[must_use]
    pub const fn deadline(&self) -> Option<UnixMillis> {
        match self.state {
            State::Live(stage) => stage.deadline(),
            State::Retired(_) => None,
        }
    }

    /// What to record beside the session's connections after every input, so
    /// an actor restored over them can reinstall this exact admission.
    ///
    /// `None` once retired: drop the recorded snapshot, keeping only what
    /// late-frame handling needs.
    #[must_use]
    pub const fn snapshot(&self) -> Option<SessionSnapshot> {
        match self.state {
            State::Live(stage) => Some(SessionSnapshot {
                key: self.key,
                stage,
            }),
            State::Retired(_) => None,
        }
    }

    /// Processes one input, checking owner revocation and expiration before it.
    ///
    /// The host serializes this with ownership changes and forwarding submission.
    /// Call even for a late timer; an active session has no traffic-idle timeout.
    pub fn handle<O: LifecycleObserver>(
        &mut self,
        lifecycle: &ShareLifecycle<O>,
        event: Event,
        now: UnixMillis,
        observer: &mut impl Observer,
    ) -> Effects {
        let before = self.phase();
        let effects = self.apply(lifecycle, event, now);
        let decision = effects.retirement.map_or_else(
            || observation::decision(effects.action, before != self.phase()),
            |retirement| Some(Decision::Retired(retirement.reason)),
        );
        if let Some(decision) = decision {
            observer.observe(Observation {
                at: now,
                session: self.key,
                event,
                before,
                after: self.phase(),
                decision,
            });
        }
        effects
    }

    /// Validates a direction before any payload arrives, so startup credit can
    /// precede the E2E handshake rather than waiting for its first frame.
    ///
    /// This does not advance receiver admission or authorize sender confirmation.
    ///
    /// # Errors
    ///
    /// Refuses a foreign lifecycle, revoked ownership, retirement or expiry.
    pub fn check_route<'a, O: LifecycleObserver>(
        &'a self,
        lifecycle: &'a ShareLifecycle<O>,
        direction: Direction,
        now: UnixMillis,
    ) -> Result<ValidatedForward<'a, O>, ForwardRefusal> {
        self.check_forward(
            lifecycle,
            ForwardPermit {
                session: self.key,
                direction,
            },
            now,
        )
    }

    /// Revalidates a queued permit against both live authorities, including time.
    ///
    /// The returned borrow spans synchronous submission. Hosts must not cache a
    /// successful check, release their authority lock, then submit after an await.
    ///
    /// # Errors
    ///
    /// Refuses different admissions, another actor, revoked owners, retirement
    /// and an expired provisional deadline even when the timer has not run.
    pub fn check_forward<'a, O: LifecycleObserver>(
        &'a self,
        lifecycle: &'a ShareLifecycle<O>,
        permit: ForwardPermit,
        now: UnixMillis,
    ) -> Result<ValidatedForward<'a, O>, ForwardRefusal> {
        if permit.session != self.key {
            return Err(ForwardRefusal::StaleSession);
        }
        lifecycle.check_session_owner(self.key.owner)?;
        if let Some(reason) = self.retired_or_expired(now) {
            return Err(ForwardRefusal::Retired(reason));
        }
        Ok(ValidatedForward {
            permit,
            _session: self,
            _lifecycle: lifecycle,
        })
    }

    fn apply<O: LifecycleObserver>(
        &mut self,
        lifecycle: &ShareLifecycle<O>,
        event: Event,
        now: UnixMillis,
    ) -> Effects {
        let ownership = lifecycle.check_session_owner(self.key.owner);
        if ownership == Err(ForwardRefusal::ForeignLifecycle) {
            return Effects {
                retirement: None,
                action: Action::Refused(Refusal::ForeignLifecycle),
            };
        }
        let invalidated = match (event, ownership) {
            // The exact revocation carries the original cause, even if another
            // transition happened before this cleanup reached the session.
            (Event::Revoke(revocation), _) if revocation.owner() == self.key.owner => None,
            (_, Err(ForwardRefusal::Retired(reason))) => Some(reason),
            _ => self.retired_or_expired(now),
        };
        let mut retirement = invalidated.and_then(|reason| self.retire(reason));
        let action = match self.validate_event(event) {
            Err(reason) => Action::Refused(reason),
            Ok(()) => {
                if let State::Retired(reason) = self.state {
                    match event {
                        Event::Tick | Event::Revoke(_) => Action::None,
                        _ => Action::Discard(reason),
                    }
                } else {
                    self.advance(event, &mut retirement)
                }
            }
        };
        Effects { retirement, action }
    }

    fn validate_event(&self, event: Event) -> Result<(), Refusal> {
        match event {
            Event::Frame { source, header } => {
                if header.session() != self.key.wire_id {
                    return Err(Refusal::StaleSession);
                }
                self.key
                    .direction(source)
                    .ok_or(Refusal::WrongParticipant)?;
            }
            Event::Admit { sender, frame } => {
                if frame.session() != self.key.wire_id {
                    return Err(Refusal::StaleSession);
                }
                if sender != self.key.sender() {
                    return Err(Refusal::WrongParticipant);
                }
            }
            Event::End {
                session,
                participant,
                reason,
            } => {
                if session != self.key {
                    return Err(Refusal::StaleSession);
                }
                if reason == EndReason::ReceiverDisconnected && participant != self.key.receiver() {
                    return Err(Refusal::WrongParticipant);
                }
                self.key
                    .direction(participant)
                    .ok_or(Refusal::WrongParticipant)?;
            }
            Event::Revoke(revocation) if revocation.owner() != self.key.owner => {
                return Err(Refusal::StaleRevocation);
            }
            Event::Revoke(_) | Event::Tick => {}
        }
        Ok(())
    }

    fn advance(&mut self, event: Event, retirement: &mut Option<Retirement>) -> Action {
        match event {
            Event::Frame { source, .. } => {
                let direction = self.key.direction(source).expect("validated participant");
                if direction == Direction::ToSender
                    && let State::Live(Stage::AwaitingReceiver { deadline }) = self.state
                {
                    self.state = State::Live(Stage::AwaitingSender { deadline });
                }
                // E2E handshake frames need routing before WS2M can be sent.
                Action::Forward(ForwardPermit {
                    session: self.key,
                    direction,
                })
            }
            Event::Admit { .. } => match self.state {
                State::Live(Stage::AwaitingReceiver { .. }) => {
                    Action::Refused(Refusal::ReceiverNotObserved)
                }
                State::Live(Stage::AwaitingSender { .. } | Stage::Active) => {
                    self.state = State::Live(Stage::Active);
                    Action::Active
                }
                State::Retired(_) => unreachable!(),
            },
            Event::End { reason, .. } => {
                *retirement = self.retire(RetirementReason::Ended(reason));
                Action::None
            }
            Event::Revoke(revocation) => {
                *retirement = self.retire(revocation.reason());
                Action::None
            }
            Event::Tick => Action::None,
        }
    }

    fn retired_or_expired(&self, now: UnixMillis) -> Option<RetirementReason> {
        if let State::Retired(reason) = self.state {
            Some(reason)
        } else if self.deadline().is_some_and(|deadline| now >= deadline) {
            Some(RetirementReason::AdmissionExpired)
        } else {
            None
        }
    }

    const fn retire(&mut self, reason: RetirementReason) -> Option<Retirement> {
        if matches!(self.state, State::Retired(_)) {
            return None;
        }
        self.state = State::Retired(reason);
        Some(Retirement {
            session: self.key,
            reason,
        })
    }
}
