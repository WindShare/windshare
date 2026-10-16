//! Single-share registration authority, independent of storage and sockets.
//!
//! Hosts serialize events, execute both parts of each `Effects`, and schedule
//! `deadline()` as an absolute timer/alarm. Every event checks that deadline,
//! so late timers never extend ownership. The engine retains only bounded
//! metadata; descriptor bytes belong to the host transaction.
//!
//! A publication that expires, loses its owner or has an uncertain result is
//! fenced. Only authoritative rollback can reopen the route. A late success
//! cannot publish it, and cleanup always names the exact operation.
//!
//! [`ShareLifecycle::restore`] is the only constructor, and it takes whatever
//! [`Record`] storage holds for the share, so an actor can never start empty
//! over a registration, unresolved publication or STOP. Hosts store exactly
//! the records `PublishRequest` and `PublishedRegistration` name.
//!
//! Published-sender disconnect grace, RESUME, STOP and receiver session
//! allocation are separate lifecycle work.

mod effect;
mod observation;
mod operation;
mod record;

pub use effect::{Action, Cancellation, Effects, Event, Expiration, PublicationResult, Refusal};
pub use observation::{Decision, Observation, ObservedEvent, Observer, Phase};
pub use operation::{
    ConnectionRef, Incarnation, Publication, PublishRequest, PublishedRegistration, RegistrationId,
    Reservation,
};
pub use record::{
    EncodedRecord, MAX_RECORD_BYTES, PublishedRoute, Record, RecordError, RestoreError,
    RouteVersion, StoppedShare, UnresolvedPublication,
};

use core::num::NonZeroU64;

use crate::{
    clock::UnixMillis,
    identifiers::{RelayIdentity, ShareId},
    registration::{RegisterInit, RegistrationMode},
};
use operation::Pending;

#[derive(Debug)]
enum State {
    Vacant,
    Reserved(Pending),
    Publishing(Pending),
    Fenced(UnresolvedPublication),
    Published(PublishedRegistration),
    // The owner's operation belonged to an earlier incarnation, so no
    // connection in this one may serve receivers until it reattaches.
    Detached(
        #[cfg_attr(
            not(test),
            expect(
                dead_code,
                reason = "RESUME and re-adoption will match the restored registration and version"
            )
        )]
        PublishedRoute,
    ),
    Stopped(StoppedShare),
}

/// One share's registration authority. No clock, random source, storage or
/// socket is owned here; platform hosts provide inputs and execute decisions.
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
    /// An unresolved publication restores fenced under its stored operation,
    /// and the returned `Action::Reconcile` must be executed like any other
    /// action. A published route restores detached, since its owner belonged
    /// to the old incarnation. `now` only timestamps the observation:
    /// restoring never derives or extends a deadline.
    ///
    /// # Errors
    ///
    /// Returns a [`RestoreError`] when the record names another relay or share,
    /// or was minted by `incarnation`.
    pub fn restore(
        relay: RelayIdentity,
        share: ShareId,
        stored: Option<Record>,
        incarnation: Incarnation,
        observer: O,
        now: UnixMillis,
    ) -> Result<(Self, Action<'static>), RestoreError> {
        let (state, action) = match stored {
            None => (State::Vacant, Action::None),
            Some(record) if record.relay() != relay => return Err(RestoreError::ForeignRelay),
            Some(record) if record.share() != share => return Err(RestoreError::ForeignShare),
            Some(Record::Unresolved(unresolved)) if unresolved.id.incarnation == incarnation => {
                return Err(RestoreError::ReusedIncarnation);
            }
            Some(Record::Unresolved(unresolved)) => {
                (State::Fenced(unresolved), Action::Reconcile(unresolved.id))
            }
            Some(Record::Published(route)) => (State::Detached(route), Action::None),
            Some(Record::Stopped(stopped)) => (State::Stopped(stopped), Action::None),
        };
        let operation = match action {
            Action::Reconcile(id) => Some(id),
            _ => None,
        };
        let mut lifecycle = Self {
            relay,
            share,
            incarnation,
            last_sequence: 0,
            state,
            observer,
        };
        let phase = lifecycle.phase();
        lifecycle.observe(
            now,
            ObservedEvent::Restore,
            operation,
            None,
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
            State::Detached(_) => Phase::Detached,
            State::Stopped(_) => Phase::Stopped,
        }
    }

    /// Returns the next absolute deadline. Fenced work needs host reconciliation,
    /// not a timeout that could forget a possibly committed record.
    #[must_use]
    pub const fn deadline(&self) -> Option<UnixMillis> {
        match self.state {
            State::Reserved(pending) | State::Publishing(pending) => Some(pending.deadline),
            State::Vacant
            | State::Fenced(_)
            | State::Published(_)
            | State::Detached(_)
            | State::Stopped(_) => None,
        }
    }

    /// Applies one event. Authentication happens in `ControlHandshake`; host
    /// publication results cannot substitute for an authenticated upload.
    pub fn handle<'f>(&mut self, event: Event<'f>, now: UnixMillis) -> Effects<'f> {
        let mut expiration = self.expire(now);
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
            Event::Join(share) => self.join(share),
            Event::Tick => Action::None,
        };
        let operation = match &action {
            Action::Reserved(reservation) => Some(reservation.id()),
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
        Effects { expiration, action }
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
            State::Stopped(stopped)
                if stopped.share_instance == init.registration().share_instance() =>
            {
                return Action::Refused(Refusal::Stopped);
            }
            // A stopped share ID also stays occupied for every other instance:
            // its single record is the permanent STOP evidence.
            _ => return Action::Refused(Refusal::Occupied),
        }
        let Some(sequence) = self.last_sequence.checked_add(1).and_then(NonZeroU64::new) else {
            return Action::Refused(Refusal::SequenceExhausted);
        };
        self.last_sequence = sequence.get();
        let pending = Pending {
            id: RegistrationId {
                relay: self.relay,
                share: self.share,
                incarnation: self.incarnation,
                connection,
                sequence,
            },
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

    fn finish(&mut self, id: RegistrationId, result: PublicationResult) -> Action<'static> {
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
                        version: RouteVersion::FIRST,
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

    fn cancel(&mut self, id: RegistrationId) -> Action<'static> {
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

    fn join(&self, share: ShareId) -> Action<'static> {
        if share != self.share {
            return Action::Refused(Refusal::WrongShare);
        }
        match self.state {
            State::Vacant => Action::Refused(Refusal::NotFound),
            State::Reserved(_) | State::Publishing(_) => Action::Refused(Refusal::Starting),
            State::Fenced(_) => Action::Refused(Refusal::PublicationUncertain),
            State::Published(published) => Action::JoinReady(published),
            State::Detached(_) => Action::Refused(Refusal::SenderDetached),
            State::Stopped(_) => Action::Refused(Refusal::Stopped),
        }
    }

    fn expire(&mut self, now: UnixMillis) -> Option<Expiration> {
        let before = self.phase();
        let (id, effect, decision) = match self.state {
            State::Reserved(pending) if now >= pending.deadline => {
                self.state = State::Vacant;
                (
                    pending.id,
                    Expiration::Released(pending.id),
                    Decision::Released,
                )
            }
            State::Publishing(pending) if now >= pending.deadline => {
                self.state = State::Fenced(pending.unresolved());
                (
                    pending.id,
                    Expiration::Reconcile(pending.id),
                    Decision::Reconcile,
                )
            }
            _ => return None,
        };
        self.observe(
            now,
            ObservedEvent::Deadline,
            Some(id),
            Some(id.connection()),
            before,
            decision,
        );
        Some(effect)
    }

    fn observe(
        &mut self,
        at: UnixMillis,
        event: ObservedEvent,
        operation: Option<RegistrationId>,
        connection: Option<ConnectionRef>,
        before: Phase,
        decision: Decision,
    ) {
        self.observer.observe(Observation {
            at,
            relay: self.relay,
            share: self.share,
            incarnation: self.incarnation,
            operation,
            connection,
            event,
            before,
            after: self.phase(),
            decision,
        });
    }
}

const fn event_context(
    event: &Event<'_>,
) -> (ObservedEvent, Option<RegistrationId>, Option<ConnectionRef>) {
    match event {
        Event::Reserve { connection, .. } => (ObservedEvent::Reserve, None, Some(*connection)),
        Event::Publish(publication) => (
            ObservedEvent::Publish,
            Some(publication.id()),
            Some(publication.id().connection()),
        ),
        Event::PublicationFinished { id, result } => (
            ObservedEvent::PublicationFinished(*result),
            Some(*id),
            Some(id.connection()),
        ),
        Event::Cancel { id, reason } => (
            ObservedEvent::Cancel(*reason),
            Some(*id),
            Some(id.connection()),
        ),
        Event::Join(_) => (ObservedEvent::Join, None, None),
        Event::Tick => (ObservedEvent::Tick, None, None),
    }
}

const fn decision(action: &Action<'_>) -> Decision {
    match action {
        Action::Reserved(_) => Decision::Reserved,
        Action::Publish(_) => Decision::Publish,
        Action::Registered(_) => Decision::Registered,
        Action::JoinReady(_) => Decision::JoinReady,
        Action::Released(_) => Decision::Released,
        Action::Reconcile(_) => Decision::Reconcile,
        Action::Refused(reason) => Decision::Refused(*reason),
        Action::None => Decision::None,
    }
}

#[cfg(test)]
#[path = "lifecycle_tests.rs"]
mod tests;
