use super::Readiness;
use super::{
    AdmissionState, ConnectionRef, Control, Error, Key, Observer, Output, RelayDecision,
    RelaySessionId, Session, ShareRelay, Turn,
};
use crate::{
    clock::UnixMillis,
    error_frame::{ErrorFrame, RetryAfter},
    flow::Grant,
    join::JoinRequest,
    lifecycle::{Event, Joined},
    sessions::{
        Action, Direction, EndReason, Event as SessionEvent, Phase, Retirement, RetirementReason,
        SessionAdmitted,
    },
};
use alloc::boxed::Box;
use alloc::vec::Vec;

const ADMISSION_RETRY: RetryAfter = match RetryAfter::from_millis(250) {
    Some(value) => value,
    None => unreachable!(),
};

impl<const N: usize, O: Observer> ShareRelay<N, O> {
    pub(super) fn join(&mut self, request: JoinRequest, now: UnixMillis, turn: &mut Turn) {
        let connection = request.connection();
        let refused = if self.readiness != Readiness::Ready {
            Some(ErrorFrame::Starting(ADMISSION_RETRY))
        } else if self.sessions.len() >= self.limits.sessions || self.last_session == u64::MAX {
            Some(ErrorFrame::Admission(ADMISSION_RETRY))
        } else {
            None
        };
        if let Some(frame) = refused {
            Self::send(turn, connection, Control::Error(frame));
            self.disconnect(connection, now, turn);
            return;
        }
        self.last_session += 1;
        let session =
            RelaySessionId::new(self.last_session.to_be_bytes()).expect("nonzero sequence");
        self.lifecycle_event(
            Event::Join { request, session },
            Some(connection),
            now,
            turn,
        );
    }

    pub(super) fn install_session(&mut self, joined: Joined, now: UnixMillis, turn: &mut Turn) {
        let key = joined.session.key();
        match self.open_directions(&joined.session, now, turn) {
            Ok(grants) => {
                for grant in grants {
                    self.routes
                        .insert((grant.flow.destination(), key.wire_id()), grant.flow);
                }
                self.deadlines
                    .set(Key::Session(key.wire_id()), joined.session.deadline());
                self.sessions.insert(
                    key.wire_id(),
                    AdmissionState {
                        session: joined.session,
                        flows: grants.map(|grant| grant.flow),
                        deferred: false,
                    },
                );
                Self::send(
                    turn,
                    key.receiver(),
                    Control::Descriptor {
                        session: key,
                        record: Box::new(joined.registration.record()),
                    },
                );
                for grant in grants {
                    Self::send(turn, grant.flow.permit().source(), Control::Grant(grant));
                }
            }
            Err(error) => {
                self.trace(Some(key.receiver()), RelayDecision::Rejected(error), now);
                Self::send(
                    turn,
                    key.receiver(),
                    Control::Error(ErrorFrame::Admission(ADMISSION_RETRY)),
                );
                self.disconnect(key.receiver(), now, turn);
            }
        }
    }

    fn open_directions(
        &mut self,
        session: &Session,
        now: UnixMillis,
        turn: &mut Turn,
    ) -> Result<[Grant; 2], Error> {
        let mut grants = Vec::new();
        for direction in [Direction::ToSender, Direction::ToReceiver] {
            let route = session
                .check_route(&self.lifecycle, direction, now)
                .map_err(Error::Forward)?;
            let destination = route.permit().destination();
            let result = self
                .connections
                .get_mut(&destination)
                .ok_or(Error::UnknownConnection)
                .and_then(|entry| entry.ledger.open(&route, now).map_err(Error::Flow));
            self.flush_ledger(destination, now, turn);
            match result {
                Ok(grant) => grants.push(grant),
                Err(error) => {
                    // No grant escaped yet, so rollback needs neither a peer notice
                    // nor an I/O acknowledgement: no payload could have been accepted.
                    let retirement = Retirement {
                        session: session.key(),
                        reason: RetirementReason::Ended(EndReason::HostFailed),
                    };
                    for grant in grants {
                        if let Some(entry) = self.connections.get_mut(&grant.flow.destination()) {
                            let _ = entry.ledger.retire(retirement, now);
                            let _ = entry.ledger.discarded(grant.flow, now);
                        }
                        self.flush_ledger(grant.flow.destination(), now, turn);
                    }
                    return Err(error);
                }
            }
        }
        Ok([grants[0], grants[1]])
    }

    pub(super) fn session_event(
        &mut self,
        id: RelaySessionId,
        event: SessionEvent,
        now: UnixMillis,
        turn: &mut Turn,
    ) -> Option<Action> {
        let deferred = self.cleanup_deferred();
        let entry = self.sessions.get_mut(&id)?;
        let effects = entry
            .session
            .handle(&self.lifecycle, event, now, &mut self.observer);
        self.deadlines
            .set(Key::Session(id), entry.session.deadline());
        if let Some(retirement) = effects.retirement {
            entry.deferred = deferred;
            let flows = entry.flows;
            for flow in flows {
                if let Some(connection) = self.connections.get_mut(&flow.destination()) {
                    let _ = connection.ledger.retire(retirement, now);
                }
                self.flush_ledger(flow.destination(), now, turn);
            }
            if !deferred {
                self.release_cleanup(id, now, turn);
            }
        }
        Some(effects.action)
    }

    pub(super) fn release_cleanup(
        &mut self,
        id: RelaySessionId,
        _now: UnixMillis,
        turn: &mut Turn,
    ) {
        if let Some(entry) = self.sessions.get_mut(&id) {
            entry.deferred = false;
            for flow in entry.flows {
                if self
                    .connections
                    .get(&flow.destination())
                    .is_some_and(|connection| connection.ledger.snapshot(flow).is_some())
                {
                    turn.outputs.push(Output::Discard(flow));
                }
            }
        }
    }

    pub(super) fn cancel_session(
        &mut self,
        participant: ConnectionRef,
        id: RelaySessionId,
        now: UnixMillis,
        turn: &mut Turn,
    ) -> Result<(), Error> {
        let key = self
            .sessions
            .get(&id)
            .ok_or(Error::UnknownSession)?
            .session
            .key();
        let action = self.session_event(
            id,
            SessionEvent::End {
                session: key,
                participant,
                reason: EndReason::Cancelled,
            },
            now,
            turn,
        );
        if matches!(action, Some(Action::Refused(_))) {
            return Err(Error::Unavailable);
        }
        Ok(())
    }

    pub(super) fn admit_session(
        &mut self,
        sender: ConnectionRef,
        frame: SessionAdmitted,
        now: UnixMillis,
        turn: &mut Turn,
    ) -> Result<(), Error> {
        match self.session_event(
            frame.session(),
            SessionEvent::Admit { sender, frame },
            now,
            turn,
        ) {
            Some(Action::Active | Action::None) => Ok(()),
            _ => Err(Error::Unavailable),
        }
    }

    pub(super) fn collect_session(
        &mut self,
        key: crate::sessions::SessionKey,
        now: UnixMillis,
        turn: &mut Turn,
    ) {
        // Feedback touches only its own two directions. Other sessions may share
        // the sender, but neither their routing nor their lifetime changes here.
        let mut retained = false;
        for destination in [key.sender(), key.receiver()] {
            let route = (destination, key.wire_id());
            if let Some(flow) = self.routes.get(&route) {
                if self
                    .connections
                    .get(&destination)
                    .is_some_and(|entry| entry.ledger.snapshot(*flow).is_some())
                {
                    retained = true;
                } else {
                    self.routes.remove(&route);
                }
            }
        }
        if retained
            || self
                .sessions
                .get(&key.wire_id())
                .is_some_and(|entry| entry.session.phase() != Phase::Retired)
        {
            return;
        }
        self.sessions.remove(&key.wire_id());
        self.deadlines.set(Key::Session(key.wire_id()), None);
        if self
            .connections
            .get(&key.receiver())
            .is_some_and(|entry| !matches!(entry.role, super::Role::Closing))
        {
            self.close(key.receiver(), turn);
            self.trace(Some(key.receiver()), RelayDecision::Closing, now);
        }
    }
}
