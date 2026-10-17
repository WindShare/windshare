//! Single-share route authority, independent of storage and sockets.
//!
//! [`crate::share::ShareRelay`] composes this state machine with sessions, delivery,
//! deadlines and ordered storage completions. Direct users must preserve the
//! ordering expressed by [`Effects`]; every event checks absolute deadlines.
//! Publication and RESUME first enter activation: only confirmation of the
//! exact ownership write makes the route serve and permits REGISTERED. The
//! original handshake deadline also bounds this final confirmation.
//!
//! A publication that expires, loses its owner or has an uncertain result stays
//! fenced until authoritative rollback. A late success cannot publish it.
//! Owner loss starts a fixed grace; RESUME replaces only the ownership observed
//! before its challenge, never an owner it did not authenticate against.
//!
//! [`ShareLifecycle::restore`] takes the authoritative [`Record`], preventing an
//! empty actor from replacing a registration, unresolved publication or STOP.
//! Surviving sockets retain their ownership and validated session identities.
//!
//! STOP revokes authority immediately, persists its cleanup obligation, retires
//! exact participants and publication work, then commits before acknowledging.
//! The aggregate retains those obligations and applies revocation to sessions.

mod activation;
mod effect;
mod observation;
mod operation;
mod record;
mod restoration;
mod session;
mod stopping;

pub use activation::OwnershipCommit;
pub use effect::{Action, Cancellation, Effects, Event, Expiration, PublicationResult, Refusal};
pub use observation::{Decision, Observation, ObservedEvent, Observer, Phase};
pub use operation::{
    ConnectionRef, Incarnation, OperationId, OwnershipEpoch, OwnershipId, Publication,
    PublishRequest, PublishedRegistration, Reservation, ResumeAttempt, Resumption,
};
pub use record::{
    EncodedRecord, GraceRoute, MAX_RECORD_BYTES, OwnedRoute, PublishedRoute, Record, RecordError,
    RestoreError, StoppedShare, UnresolvedPublication,
};
pub use restoration::{Restoration, SessionRestore};
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
    Activating(OwnershipCommit),
    Published(PublishedRegistration),
    // Keeps the lost ownership, so a resume admitted while that owner was
    // live still commits after it disconnects.
    Grace(GraceRoute),
    Stopping(StopWork),
    Stopped(StoppedShare),
}

/// The live or lost ownership a resume would replace.
#[derive(Clone, Copy)]
struct Incumbent {
    ownership: OwnershipId,
    route: PublishedRoute,
    live: bool,
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
    pub(crate) const fn observer_mut(&mut self) -> &mut O {
        &mut self.observer
    }

    /// Constructs the share actor for `relay` and `share` from `restoration`:
    /// the stored record and whatever outlived the previous actor. Every
    /// restore, including a replacement inside one process, needs a fresh
    /// `incarnation`; it fences old host callbacks, never live authority.
    ///
    /// An unresolved publication restores fenced under its stored operation,
    /// and the returned `Action::Reconcile` must be executed like any other
    /// action. A published route re-adopts its stored ownership when the owner
    /// survived, without a write; otherwise it returns `Action::Detached`, the
    /// only deadline a restore derives, which the host persists so no later
    /// restore extends it. A stored grace keeps its deadline, even a past one
    /// that the next event or timer lapses. A stopping record resumes exact
    /// cleanup, including surviving connections, before final persistence and
    /// STOPPED.
    ///
    /// Before returning, `install` receives one [`SessionRestore`] for each
    /// surviving session snapshot, by its index. Only a snapshot of the
    /// re-adopted ownership whose receiver survived, whose admission is still
    /// within its original deadline and that no sibling contradicts becomes a
    /// `Session` again, keeping its permits; every other one is retired alone.
    /// Delivery ledger restoration, still to come, belongs after this call:
    /// open flows then revalidate against reinstated sessions, and a retired
    /// snapshot's flows settle like any other retirement.
    ///
    /// # Errors
    ///
    /// Returns a [`RestoreError`] when the record names another relay or share,
    /// or was minted by `incarnation`.
    pub fn restore(
        relay: RelayIdentity,
        share: ShareId,
        restoration: Restoration<'_>,
        incarnation: Incarnation,
        observer: O,
        now: UnixMillis,
        install: impl FnMut(usize, SessionRestore),
    ) -> Result<(Self, Action<'static>), RestoreError> {
        match restoration.record {
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
        let (action, operation, connection) =
            lifecycle.install(restoration.record, restoration.connections, now);
        let phase = lifecycle.phase();
        lifecycle.observe(
            now,
            ObservedEvent::Restore,
            operation,
            connection,
            phase,
            decision(&action),
        );
        lifecycle.restore_sessions(&restoration, now, install);
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
            State::Activating(_) => Phase::Activating,
            State::Published(_) => Phase::Published,
            State::Grace(_) => Phase::Grace,
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
            State::Activating(commit) => Some(commit.deadline),
            State::Grace(route) => Some(route.deadline),
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
            Event::OwnershipCommitted(id) => self.ownership_committed(id),
            Event::Cancel { id, .. } => self.cancel(id, now),
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
            Action::CommitOwnership(commit) => Some(commit.id()),
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
        let (unresolved, deadline) = match self.state {
            State::Publishing(pending) if pending.id == id => {
                (pending.unresolved(), Some(pending.deadline))
            }
            State::Fenced(unresolved) if unresolved.id == id => (unresolved, None),
            _ => return Action::Refused(Refusal::StaleOperation),
        };
        match (result, deadline) {
            (PublicationResult::Aborted, _) => {
                self.state = State::Vacant;
                Action::Released(id)
            }
            (PublicationResult::Committed, Some(deadline)) => {
                let route = PublishedRoute {
                    relay: self.relay,
                    registration: unresolved.registration,
                };
                let published = PublishedRegistration {
                    owner: route.ownership(OwnershipEpoch::FIRST, id.connection),
                    route,
                };
                self.commit_ownership(id, published, deadline, None)
            }
            (PublicationResult::Committed | PublicationResult::Uncertain, _) => {
                self.state = State::Fenced(unresolved);
                Action::Reconcile(id)
            }
        }
    }

    fn cancel(&mut self, id: OperationId, now: UnixMillis) -> Action<'static> {
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
            State::Activating(commit) if commit.id == id => self.detach(id.connection(), now),
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
        let ownership = match self.incumbent(init.registration()) {
            Ok(incumbent) => incumbent.map(|incumbent| incumbent.ownership),
            Err(refusal) => return Action::Refused(refusal),
        };
        if matches!(self.state, State::Activating(_)) {
            return Action::Refused(Refusal::Starting);
        }
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
        let current = match self.incumbent(attempt.init.registration()) {
            Ok(Some(current)) => current,
            Ok(None) => return Action::Refused(Refusal::NotFound),
            Err(refusal) => return Action::Refused(refusal),
        };
        // The durable ownership, not this actor's incarnation, fences the
        // commit: a restore over the observed owner left nothing to protect.
        if attempt.ownership != Some(current.ownership) {
            return Action::Refused(Refusal::ResumeStale);
        }
        let Some(epoch) = current.ownership.epoch.next() else {
            return Action::Refused(Refusal::SequenceExhausted);
        };
        let registration = PublishedRegistration {
            owner: current.route.ownership(epoch, attempt.id.connection),
            route: current.route,
        };
        self.commit_ownership(
            attempt.id,
            registration,
            attempt.deadline,
            current.live.then_some(current.ownership.connection),
        )
    }

    /// Returns the ownership a resume of `registration` would replace now,
    /// `None` when no route exists, or why the share cannot be resumed.
    fn incumbent(&self, registration: &Registration) -> Result<Option<Incumbent>, Refusal> {
        let (held, incumbent) = match self.state {
            State::Vacant => return Ok(None),
            State::Reserved(pending) | State::Publishing(pending) => {
                (*pending.init.registration(), Err(Refusal::Starting))
            }
            State::Fenced(unresolved) => {
                (unresolved.registration, Err(Refusal::PublicationUncertain))
            }
            State::Activating(commit) => (
                commit.route.route.registration,
                Ok(Incumbent {
                    ownership: commit.route.ownership(),
                    route: commit.route.route,
                    live: true,
                }),
            ),
            State::Published(published) => (
                published.route.registration,
                Ok(Incumbent {
                    ownership: published.owner,
                    route: published.route,
                    live: true,
                }),
            ),
            State::Grace(route) => (
                route.route.registration,
                Ok(Incumbent {
                    ownership: route.lost(),
                    route: route.route,
                    live: false,
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
        incumbent.map(Some)
    }

    fn detach(&mut self, owner: ConnectionRef, now: UnixMillis) -> Action<'static> {
        let published = match self.state {
            State::Published(published) => published,
            State::Activating(commit) => commit.registration(),
            _ => return Action::Refused(Refusal::StaleOperation),
        };
        if published.owner.connection != owner {
            return Action::Refused(Refusal::StaleOperation);
        }
        let route = GraceRoute::new(
            published.route,
            published.owner,
            now.saturating_add(SENDER_GRACE),
        );
        self.state = State::Grace(route);
        Action::Detached(route)
    }

    fn join(&self, share: ShareId) -> Action<'static> {
        if share != self.share {
            return Action::Refused(Refusal::WrongShare);
        }
        match self.state {
            State::Vacant => Action::Refused(Refusal::NotFound),
            State::Reserved(_) | State::Publishing(_) | State::Activating(_) => {
                Action::Refused(Refusal::Starting)
            }
            State::Fenced(_) => Action::Refused(Refusal::PublicationUncertain),
            State::Published(published) => Action::JoinReady(published),
            State::Grace(_) => Action::Refused(Refusal::SenderDetached),
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
            State::Activating(commit) if now >= commit.deadline => {
                let registration = commit.registration();
                let grace = GraceRoute::new(
                    registration.route,
                    registration.owner,
                    now.saturating_add(SENDER_GRACE),
                );
                self.state = State::Grace(grace);
                (
                    Some(commit.id),
                    Expiration::Detached(grace),
                    Decision::Detached,
                )
            }
            State::Grace(route) if now >= route.deadline => {
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

    /// Issues the next attempt or admission sequence; it never wraps.
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
