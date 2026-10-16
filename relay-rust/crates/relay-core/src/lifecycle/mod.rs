//! Single-share route authority, independent of storage and sockets.
//!
//! Hosts serialize events, apply every part of each `Effects`, and schedule
//! `deadline()` as an absolute timer/alarm. Every event checks that deadline,
//! so late timers never extend ownership. The engine retains only bounded
//! metadata; descriptor bytes belong to the host transaction. Hosts finish
//! record/cleanup effects without a completion event before applying another
//! input. Publication and STOP work instead report their explicit completions;
//! all asynchronous writes and deletes are conditional on the exact record.
//!
//! A publication that expires, loses its owner or has an uncertain result is
//! fenced. Only authoritative rollback can reopen the route. A late success
//! cannot publish it, and cleanup always names the exact operation.
//!
//! A published route has at most one owner connection. Losing it without STOP
//! starts a grace whose absolute deadline the host persists, and the route
//! lapses unless its sender resumes in time. An authenticated RESUME replaces
//! a live or lost owner, but only the ownership it observed before its
//! challenge, so it never retires an owner it did not authenticate against.
//!
//! [`ShareLifecycle::restore`] is the only constructor, and it takes whatever
//! [`Record`] storage holds for the share, so an actor can never start empty
//! over a registration, unresolved publication or STOP. Hosts store exactly
//! the records lifecycle outputs name.
//!
//! STOP fences the route immediately, persists its cleanup obligation, retires
//! exact participants and outstanding publication work, then commits the final
//! record before acknowledging. Receiver JOIN creates a session bound to this
//! ownership; revocations name the exact old ownership for session cleanup.
//! Hosts retain session authorities in their own bounded resource storage.

mod effect;
mod observation;
mod operation;
mod record;
mod session;
mod stopping;

pub use effect::{Action, Cancellation, Effects, Event, Expiration, PublicationResult, Refusal};
pub use observation::{Decision, Observation, ObservedEvent, Observer, Phase};
pub use operation::{
    ConnectionRef, Incarnation, OperationId, Publication, PublishRequest, PublishedRegistration,
    Reservation, ResumeAttempt, Resumed, Resumption,
};
pub use record::{
    EncodedRecord, GraceRoute, MAX_RECORD_BYTES, OwnedRoute, PublishedRoute, Record, RecordError,
    RestoreError, StoppedShare, UnresolvedPublication,
};
pub use session::Joined;
pub use stopping::{PendingStop, StopAcknowledgement, StopCleanup, StopResult, StopStep, StopWork};

use core::{num::NonZeroU64, time::Duration};

use crate::{
    clock::UnixMillis,
    identifiers::{RelayIdentity, ShareId},
    registration::{RegisterInit, Registration, RegistrationMode},
};
use observation::{decision, event_context};
use operation::Pending;

/// How long a published route waits for its sender after losing the owner
/// connection: enough for a sender to notice the loss and redial through its
/// relay backoff, short enough that a crashed sender does not keep receivers
/// retrying a dead share.
const SENDER_GRACE: Duration = Duration::from_secs(60);

#[derive(Debug)]
enum State {
    Vacant,
    Reserved(Pending),
    Publishing(Pending),
    Fenced(UnresolvedPublication),
    // The registration's sequence identifies this ownership.
    Published(PublishedRegistration),
    // Keeps the lost owner's sequence, so a resume admitted while that owner
    // was live still commits after it disconnects.
    Grace {
        ownership: NonZeroU64,
        route: GraceRoute,
    },
    Stopping(StopWork),
    Stopped(StoppedShare),
}

/// The ownership a resume would replace.
#[derive(Clone, Copy)]
struct Ownership {
    sequence: NonZeroU64,
    route: PublishedRoute,
    owner: Option<ConnectionRef>,
}

/// One share's route authority. No clock, random source, storage or socket is
/// owned here; platform hosts provide inputs and execute decisions.
#[derive(Debug)]
pub struct ShareLifecycle<O> {
    relay: RelayIdentity,
    share: ShareId,
    incarnation: Incarnation,
    last_sequence: u64,
    state: State,
    observer: O,
}

impl<O: Observer> ShareLifecycle<O> {
    /// Constructs the share actor from what storage holds for `relay` and
    /// `share`: `None` only when no record exists. Every restore, including a
    /// replacement inside one process, needs a fresh `incarnation`.
    ///
    /// `surviving` lists the share's connections that outlived the previous
    /// incarnation, such as hibernated sockets; it is empty after a process
    /// restart. Hosts whose connections survive must never reuse a
    /// `ConnectionRef` across incarnations, since re-adoption trusts it.
    ///
    /// An unresolved publication restores fenced under its stored operation,
    /// and the returned `Action::Reconcile` must be executed like any other
    /// action. A published route re-adopts its stored owner when it survived,
    /// without a write; otherwise it returns `Action::Detached`, the only
    /// deadline a restore derives, which the host persists so no later restore
    /// extends it. A stored grace keeps its deadline, even a past one that the
    /// next event or timer lapses. A stopping record resumes exact cleanup,
    /// including surviving connections, before final persistence and STOPPED.
    ///
    /// # Errors
    ///
    /// Returns a [`RestoreError`] when the record names another relay or share,
    /// or was minted by `incarnation`.
    pub fn restore(
        relay: RelayIdentity,
        share: ShareId,
        stored: Option<Record>,
        surviving: impl IntoIterator<Item = ConnectionRef>,
        incarnation: Incarnation,
        observer: O,
        now: UnixMillis,
    ) -> Result<(Self, Action<'static>), RestoreError> {
        match stored {
            Some(record) if record.relay() != relay => return Err(RestoreError::ForeignRelay),
            Some(record) if record.share() != share => return Err(RestoreError::ForeignShare),
            Some(Record::Unresolved(unresolved)) if unresolved.id.incarnation == incarnation => {
                return Err(RestoreError::ReusedIncarnation);
            }
            Some(Record::Stopping(pending))
                if pending.id().incarnation() == incarnation
                    || pending
                        .cleanup()
                        .operation()
                        .is_some_and(|id| id.incarnation() == incarnation) =>
            {
                return Err(RestoreError::ReusedIncarnation);
            }
            _ => {}
        }
        let mut lifecycle = Self {
            relay,
            share,
            incarnation,
            last_sequence: 0,
            state: State::Vacant,
            observer,
        };
        let (action, operation, connection) = lifecycle.install(stored, surviving, now);
        let phase = lifecycle.phase();
        lifecycle.observe(
            now,
            ObservedEvent::Restore,
            operation,
            connection,
            phase,
            decision(&action),
        );
        Ok((lifecycle, action))
    }

    /// Returns lifecycle visibility without exposing mutable state.
    #[must_use]
    pub const fn phase(&self) -> Phase {
        match self.state {
            State::Vacant => Phase::Vacant,
            State::Reserved(_) => Phase::Reserved,
            State::Publishing(_) => Phase::Publishing,
            State::Fenced(_) => Phase::Fenced,
            State::Published(_) => Phase::Published,
            State::Grace { .. } => Phase::Grace,
            State::Stopping(_) => Phase::Stopping,
            State::Stopped(_) => Phase::Stopped,
        }
    }

    /// Returns the next absolute deadline. Fenced work needs host reconciliation,
    /// not a timeout that could forget a possibly committed record.
    #[must_use]
    pub const fn deadline(&self) -> Option<UnixMillis> {
        match self.state {
            State::Reserved(pending) | State::Publishing(pending) => Some(pending.deadline),
            State::Grace { route, .. } => Some(route.deadline),
            State::Vacant
            | State::Fenced(_)
            | State::Published(_)
            | State::Stopping(_)
            | State::Stopped(_) => None,
        }
    }

    /// Applies one event. Authentication happens in `ControlHandshake`; host
    /// publication results cannot substitute for an authenticated upload.
    pub fn handle<'f>(&mut self, event: Event<'f>, now: UnixMillis) -> Effects<'f> {
        // STOP must retain the exact cleanup obligation even at an expired
        // reservation/grace boundary. A refused STOP still runs normal expiry.
        let owner_before = self.session_owner();
        let is_stop = matches!(event, Event::Stop { .. });
        let mut expiration = if is_stop { None } else { self.expire(now) };
        let before = self.phase();
        let (observed, operation, connection) = event_context(&event);
        let action = match event {
            Event::Reserve {
                connection,
                init,
                deadline,
            } => self.reserve(connection, init, deadline, now),
            Event::Publish(publication) => self.publish(publication),
            Event::PublicationFinished { id, result } => self.finish(id, result),
            Event::Cancel { id, .. } => self.cancel(id),
            Event::BeginResume {
                connection,
                init,
                deadline,
            } => self.begin_resume(connection, init, deadline, now),
            Event::Resume(resumption) => self.resume(resumption, now),
            Event::OwnerDisconnected(owner) => self.detach(owner, now),
            Event::Stop {
                connection,
                authority,
            } => self.stop(connection, authority),
            Event::StopFinished { work, result } => self.finish_stop(work, result),
            Event::CheckJoin(share) => self.join(share),
            Event::Join { request, session } => self.join_session(request, session, now),
            Event::Tick => Action::None,
        };
        let operation = match &action {
            Action::Reserved(reservation) => Some(reservation.id()),
            Action::ResumeAdmitted(attempt) => Some(attempt.id()),
            Action::Joined(joined) => Some(joined.session.key().admission()),
            Action::Stop(work) => Some(work.pending().id()),
            _ => operation,
        };
        self.observe(
            now,
            observed,
            operation,
            connection,
            before,
            decision(&action),
        );
        // A completion discovered after expiry either confirms rollback or
        // already requests reconciliation. Retain only unrelated expiry work.
        if let (
            Some(Expiration::Reconcile(expired)),
            Action::Reconcile(current) | Action::Released(current),
        ) = (expiration, &action)
            && expired == *current
        {
            expiration = None;
        }
        if is_stop && matches!(action, Action::Refused(_)) {
            expiration = self.expire(now);
        }
        let revocation = self.session_revocation(owner_before);
        Effects {
            revocation,
            expiration,
            action,
        }
    }

    /// Installs the stored state and returns the host action it requires,
    /// with the operation and connection the restore observation names.
    fn install(
        &mut self,
        stored: Option<Record>,
        surviving: impl IntoIterator<Item = ConnectionRef>,
        now: UnixMillis,
    ) -> (Action<'static>, Option<OperationId>, Option<ConnectionRef>) {
        match stored {
            None => (Action::None, None, None),
            Some(Record::Unresolved(unresolved)) => {
                self.state = State::Fenced(unresolved);
                let id = unresolved.id;
                (Action::Reconcile(id), Some(id), Some(id.connection))
            }
            Some(Record::Published(owned)) => {
                let ownership = self.restored_ownership();
                if surviving
                    .into_iter()
                    .any(|connection| connection == owned.owner)
                {
                    let id = self.operation(owned.owner, ownership);
                    self.state = State::Published(PublishedRegistration {
                        id,
                        route: owned.route,
                    });
                    return (Action::None, Some(id), Some(owned.owner));
                }
                let route = GraceRoute {
                    route: owned.route,
                    deadline: now.saturating_add(SENDER_GRACE),
                };
                self.state = State::Grace { ownership, route };
                (Action::Detached(route), None, Some(owned.owner))
            }
            Some(Record::Grace(route)) => {
                self.state = State::Grace {
                    ownership: self.restored_ownership(),
                    route,
                };
                (Action::None, None, None)
            }
            Some(Record::Stopping(pending)) => {
                // The durable obligation survives hibernation and process
                // restart. Repeating exact cleanup is safe; re-adoption is not.
                let work = StopWork::Retire(pending);
                self.state = State::Stopping(work);
                (
                    Action::Stop(work),
                    Some(pending.id()),
                    Some(pending.id().connection()),
                )
            }
            Some(Record::Stopped(stopped)) => {
                self.state = State::Stopped(stopped);
                (Action::None, None, None)
            }
        }
    }

    /// A restored route takes this incarnation's first sequence as its
    /// ownership. Resumes admitted by an earlier incarnation may carry the
    /// same sequence, so commits also compare the incarnation.
    const fn restored_ownership(&mut self) -> NonZeroU64 {
        self.last_sequence = NonZeroU64::MIN.get();
        NonZeroU64::MIN
    }

    fn reserve(
        &mut self,
        connection: ConnectionRef,
        init: RegisterInit,
        deadline: UnixMillis,
        now: UnixMillis,
    ) -> Action<'static> {
        if init.registration().route().share_id() != self.share {
            return Action::Refused(Refusal::WrongShare);
        }
        if init.mode() != RegistrationMode::Fresh {
            return Action::Refused(Refusal::UnsupportedMode);
        }
        if now >= deadline {
            return Action::Refused(Refusal::Expired);
        }
        match self.state {
            State::Vacant => {}
            State::Fenced(_) => return Action::Refused(Refusal::PublicationUncertain),
            State::Stopping(_) => return Action::Refused(Refusal::StopPending),
            State::Stopped(stopped)
                if stopped.share_instance == init.registration().share_instance() =>
            {
                return Action::Refused(Refusal::Stopped);
            }
            // A stopped share ID also stays occupied for every other instance:
            // its single record is the permanent STOP evidence.
            _ => return Action::Refused(Refusal::Occupied),
        }
        let Some(sequence) = self.next_sequence() else {
            return Action::Refused(Refusal::SequenceExhausted);
        };
        let pending = Pending {
            id: self.operation(connection, sequence),
            init,
            deadline,
        };
        self.state = State::Reserved(pending);
        Action::Reserved(Reservation(pending))
    }

    #[expect(
        clippy::needless_pass_by_value,
        reason = "publication consumes the authenticated upload capability"
    )]
    fn publish<'f>(&mut self, publication: Publication<'f>) -> Action<'f> {
        let State::Reserved(pending) = self.state else {
            return Action::Refused(Refusal::StaleOperation);
        };
        if pending.id != publication.id
            || pending.init != *publication.descriptor.registration().init()
            || pending.deadline != publication.deadline
        {
            return Action::Refused(Refusal::StaleOperation);
        }
        self.state = State::Publishing(pending);
        Action::Publish(PublishRequest {
            id: pending.id,
            init: pending.init,
            descriptor: publication.descriptor.object(),
            deadline: pending.deadline,
        })
    }

    fn finish(&mut self, id: OperationId, result: PublicationResult) -> Action<'static> {
        let (unresolved, publishing) = match self.state {
            State::Publishing(pending) if pending.id == id => (pending.unresolved(), true),
            State::Fenced(unresolved) if unresolved.id == id => (unresolved, false),
            _ => return Action::Refused(Refusal::StaleOperation),
        };
        match result {
            PublicationResult::Aborted => {
                self.state = State::Vacant;
                Action::Released(id)
            }
            PublicationResult::Committed if publishing => {
                let published = PublishedRegistration {
                    id,
                    route: PublishedRoute {
                        relay: self.relay,
                        registration: unresolved.registration,
                    },
                };
                self.state = State::Published(published);
                Action::Registered(published)
            }
            PublicationResult::Committed | PublicationResult::Uncertain => {
                self.state = State::Fenced(unresolved);
                Action::Reconcile(id)
            }
        }
    }

    fn cancel(&mut self, id: OperationId) -> Action<'static> {
        match self.state {
            State::Reserved(pending) if pending.id == id => {
                self.state = State::Vacant;
                Action::Released(id)
            }
            State::Publishing(pending) if pending.id == id => {
                self.state = State::Fenced(pending.unresolved());
                Action::Reconcile(id)
            }
            State::Fenced(unresolved) if unresolved.id == id => Action::Reconcile(id),
            _ => Action::Refused(Refusal::StaleOperation),
        }
    }

    fn begin_resume(
        &mut self,
        connection: ConnectionRef,
        init: RegisterInit,
        deadline: UnixMillis,
        now: UnixMillis,
    ) -> Action<'static> {
        if init.registration().route().share_id() != self.share {
            return Action::Refused(Refusal::WrongShare);
        }
        if init.mode() != RegistrationMode::Resume {
            return Action::Refused(Refusal::UnsupportedMode);
        }
        if now >= deadline {
            return Action::Refused(Refusal::Expired);
        }
        // An absent route still spends a challenge: only an authenticated
        // sender may learn the `NotFound` that permits publishing again.
        let ownership = match self.ownership(init.registration()) {
            Ok(ownership) => ownership.map(|ownership| ownership.sequence),
            Err(refusal) => return Action::Refused(refusal),
        };
        let Some(sequence) = self.next_sequence() else {
            return Action::Refused(Refusal::SequenceExhausted);
        };
        Action::ResumeAdmitted(ResumeAttempt {
            id: self.operation(connection, sequence),
            init,
            deadline,
            ownership,
        })
    }

    fn resume(&mut self, resumption: Resumption, now: UnixMillis) -> Action<'static> {
        let attempt = resumption.attempt;
        if attempt.id.relay != self.relay || attempt.id.share != self.share {
            return Action::Refused(Refusal::StaleOperation);
        }
        if now >= attempt.deadline {
            return Action::Refused(Refusal::Expired);
        }
        let current = match self.ownership(attempt.init.registration()) {
            Ok(Some(current)) => current,
            Ok(None) => return Action::Refused(Refusal::NotFound),
            Err(refusal) => return Action::Refused(refusal),
        };
        if attempt.id.incarnation != self.incarnation || attempt.ownership != Some(current.sequence)
        {
            return Action::Refused(Refusal::ResumeStale);
        }
        let registration = PublishedRegistration {
            id: attempt.id,
            route: current.route,
        };
        self.state = State::Published(registration);
        Action::Resumed(Resumed {
            registration,
            replaced: current.owner,
        })
    }

    /// Returns the ownership a resume of `registration` would replace now,
    /// `None` when no route exists, or why the share cannot be resumed.
    fn ownership(&self, registration: &Registration) -> Result<Option<Ownership>, Refusal> {
        let (held, ownership) = match self.state {
            State::Vacant => return Ok(None),
            State::Reserved(pending) | State::Publishing(pending) => {
                (*pending.init.registration(), Err(Refusal::Starting))
            }
            State::Fenced(unresolved) => {
                (unresolved.registration, Err(Refusal::PublicationUncertain))
            }
            State::Published(published) => (
                published.route.registration,
                Ok(Ownership {
                    sequence: published.id.sequence,
                    route: published.route,
                    owner: Some(published.id.connection),
                }),
            ),
            State::Grace { ownership, route } => (
                route.route.registration,
                Ok(Ownership {
                    sequence: ownership,
                    route: route.route,
                    owner: None,
                }),
            ),
            State::Stopping(_) => return Err(Refusal::StopPending),
            State::Stopped(stopped) => {
                let retired = stopped.route == registration.route()
                    && stopped.share_instance == registration.share_instance();
                return Err(if retired {
                    Refusal::Stopped
                } else {
                    Refusal::ResumeMismatch
                });
            }
        };
        if held != *registration {
            return Err(Refusal::ResumeMismatch);
        }
        ownership.map(Some)
    }

    fn detach(&mut self, owner: ConnectionRef, now: UnixMillis) -> Action<'static> {
        let State::Published(published) = self.state else {
            return Action::Refused(Refusal::StaleOperation);
        };
        if published.id.connection != owner {
            return Action::Refused(Refusal::StaleOperation);
        }
        let route = GraceRoute {
            route: published.route,
            deadline: now.saturating_add(SENDER_GRACE),
        };
        self.state = State::Grace {
            ownership: published.id.sequence,
            route,
        };
        Action::Detached(route)
    }

    fn join(&self, share: ShareId) -> Action<'static> {
        if share != self.share {
            return Action::Refused(Refusal::WrongShare);
        }
        match self.state {
            State::Vacant => Action::Refused(Refusal::NotFound),
            State::Reserved(_) | State::Publishing(_) => Action::Refused(Refusal::Starting),
            State::Fenced(_) => Action::Refused(Refusal::PublicationUncertain),
            State::Published(published) => Action::JoinReady(published),
            State::Grace { .. } => Action::Refused(Refusal::SenderDetached),
            State::Stopping(_) => Action::Refused(Refusal::StopPending),
            State::Stopped(_) => Action::Refused(Refusal::Stopped),
        }
    }

    fn expire(&mut self, now: UnixMillis) -> Option<Expiration> {
        let before = self.phase();
        let (operation, effect, decision) = match self.state {
            State::Reserved(pending) if now >= pending.deadline => {
                self.state = State::Vacant;
                (
                    Some(pending.id),
                    Expiration::Released(pending.id),
                    Decision::Released,
                )
            }
            State::Publishing(pending) if now >= pending.deadline => {
                self.state = State::Fenced(pending.unresolved());
                (
                    Some(pending.id),
                    Expiration::Reconcile(pending.id),
                    Decision::Reconcile,
                )
            }
            State::Grace { route, .. } if now >= route.deadline => {
                self.state = State::Vacant;
                (None, Expiration::Lapsed(route.route), Decision::Lapsed)
            }
            _ => return None,
        };
        self.observe(
            now,
            ObservedEvent::Deadline,
            operation,
            operation.map(OperationId::connection),
            before,
            decision,
        );
        Some(effect)
    }

    /// Issues the next attempt or ownership sequence; it never wraps.
    fn next_sequence(&mut self) -> Option<NonZeroU64> {
        let sequence = self
            .last_sequence
            .checked_add(1)
            .and_then(NonZeroU64::new)?;
        self.last_sequence = sequence.get();
        Some(sequence)
    }

    const fn operation(&self, connection: ConnectionRef, sequence: NonZeroU64) -> OperationId {
        OperationId {
            relay: self.relay,
            share: self.share,
            incarnation: self.incarnation,
            connection,
            sequence,
        }
    }
}

#[cfg(test)]
#[path = "lifecycle_tests.rs"]
mod tests;
