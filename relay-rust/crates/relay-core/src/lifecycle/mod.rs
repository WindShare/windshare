//! Single-share fresh registration, independent of persistence and sockets.
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
//! This stage ends at publication. Published-sender disconnect grace, RESUME,
//! permanent STOP and receiver session allocation are separate lifecycle work;
//! hosts must not use a fresh engine to bypass an existing durable record.

mod effect;
mod observation;
mod operation;

pub use effect::{Action, Cancellation, Effects, Event, Expiration, PublicationResult, Refusal};
pub use observation::{Decision, Observation, ObservedEvent, Observer, Phase};
pub use operation::{
    ConnectionRef, Incarnation, Publication, PublishRequest, PublishedRegistration, RegistrationId,
    Reservation,
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
    Fenced(Pending),
    Published(PublishedRegistration),
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
    /// Creates a vacant share actor after the host has established that no
    /// durable registration, unresolved transaction or permanent STOP exists.
    /// Replacements must use a fresh incarnation to fence old callbacks.
    pub const fn vacant(
        relay: RelayIdentity,
        share: ShareId,
        incarnation: Incarnation,
        observer: O,
    ) -> Self {
        Self {
            relay,
            share,
            incarnation,
            last_sequence: 0,
            state: State::Vacant,
            observer,
        }
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
        }
    }

    /// Returns the next absolute deadline. Fenced work needs host reconciliation,
    /// not a timeout that could forget a possibly committed record.
    #[must_use]
    pub const fn deadline(&self) -> Option<UnixMillis> {
        match self.state {
            State::Reserved(pending) | State::Publishing(pending) => Some(pending.deadline),
            State::Vacant | State::Fenced(_) | State::Published(_) => None,
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
        if init.route().share_id() != self.share {
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
        let (State::Publishing(pending) | State::Fenced(pending)) = self.state else {
            return Action::Refused(Refusal::StaleOperation);
        };
        if pending.id != id {
            return Action::Refused(Refusal::StaleOperation);
        }
        match result {
            PublicationResult::Aborted => {
                self.state = State::Vacant;
                Action::Released(id)
            }
            PublicationResult::Committed if matches!(self.state, State::Publishing(_)) => {
                let published = PublishedRegistration {
                    id,
                    init: pending.init,
                };
                self.state = State::Published(published);
                Action::Registered(published)
            }
            PublicationResult::Committed | PublicationResult::Uncertain => {
                self.state = State::Fenced(pending);
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
            State::Publishing(pending) | State::Fenced(pending) if pending.id == id => {
                self.state = State::Fenced(pending);
                Action::Reconcile(id)
            }
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
                self.state = State::Fenced(pending);
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
