use crate::share::{
    ConnectionRef, Control, Error, Observer, Output, RelayDecision, RelaySessionId, Session,
    ShareRelay, Turn,
};
use crate::{
    clock::UnixMillis,
    error_frame::{ErrorFrame, RetryAfter},
    flow::Grant,
    join::JoinRequest,
    lifecycle::{Event, Joined},
    observation::SessionInputKind,
    sessions::{
        Action, Direction, EndReason, Event as SessionEvent, Retirement, RetirementReason,
        SessionAdmitted,
    },
};
use alloc::boxed::Box;
use alloc::vec::Vec;

const ADMISSION_RETRY: RetryAfter = match RetryAfter::from_millis(250) {
    Some(value) => value,
    None => unreachable!(),
};

impl<O: Observer> ShareRelay<O> {
    pub(super) fn join(&mut self, request: JoinRequest, now: UnixMillis, turn: &mut Turn) {
        let connection = request.connection();
        if !self.declare_capacity(connection, request.receive_capacity(), now, turn) {
            return;
        }
        let Ok(session) = self.sessions.allocate(self.limits.sessions) else {
            Self::send(
                turn,
                connection,
                Control::Error(ErrorFrame::Admission(ADMISSION_RETRY)),
            );
            self.disconnect(connection, now, turn);
            return;
        };
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
                self.sessions
                    .install(joined.session, &grants.map(|grant| grant.flow));
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
                .generations
                .update_ledger(
                    destination,
                    |ledger| ledger.open(&route, now),
                    &mut self.observer,
                )
                .ok_or(Error::UnknownConnection)
                .and_then(|result| result.map_err(Error::Flow));
            self.apply_delivery_close(destination, now, turn);
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
                        self.generations.update_ledger(
                            grant.flow.destination(),
                            |ledger| {
                                let _ = ledger.retire(retirement, now);
                                let _ = ledger.discarded(grant.flow, now);
                            },
                            &mut self.observer,
                        );
                        self.apply_delivery_close(grant.flow.destination(), now, turn);
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
        let effects = self.sessions.handle(
            id,
            &self.lifecycle,
            event,
            deferred,
            now,
            &mut self.observer,
        )?;
        if let Some(retirement) = effects.retirement {
            let flows = self.sessions.entries()[&id].flows;
            for flow in flows {
                let _ = self.generations.update_ledger(
                    flow.destination(),
                    |ledger| ledger.retire(retirement, now),
                    &mut self.observer,
                );
                self.apply_delivery_close(flow.destination(), now, turn);
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
        if let Some(flows) = self.sessions.release_cleanup(id) {
            for flow in flows {
                if self
                    .generations
                    .ledger(&flow.destination())
                    .is_some_and(|ledger| ledger.snapshot(flow).is_some())
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
        let Some(entry) = self.sessions.entries().get(&id) else {
            return self.discard_collected_session(
                participant,
                id,
                SessionInputKind::Cancellation,
                now,
            );
        };
        let key = entry.session.key();
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
        if let Some(Action::Refused(reason)) = action {
            return Err(Error::Session(reason));
        }
        if matches!(action, Some(Action::None)) {
            self.accepted_inbound(participant, now);
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
            Some(Action::Active) => {
                self.accepted_inbound(sender, now);
                Ok(())
            }
            Some(Action::Discard(_)) => Ok(()),
            Some(Action::Refused(reason)) => Err(Error::Session(reason)),
            None => self.discard_collected_session(
                sender,
                frame.session(),
                SessionInputKind::Admission,
                now,
            ),
            Some(Action::Forward(_) | Action::None) => {
                unreachable!("admission must activate or be rejected")
            }
        }
    }

    pub(super) fn discard_collected_session(
        &mut self,
        source: ConnectionRef,
        id: RelaySessionId,
        kind: SessionInputKind,
        now: UnixMillis,
    ) -> Result<(), Error> {
        // Collection can precede frames already in flight on the shared sender.
        // The persisted, never-reused ID watermark avoids unbounded tombstones.
        // No route, credit or liveness is restored by discarding these inputs.
        if u64::from_be_bytes(*id.as_bytes()) > self.sessions.last_issued() {
            return Err(Error::UnknownSession);
        }
        self.trace(
            Some(source),
            RelayDecision::CollectedSessionInput { session: id, kind },
            now,
        );
        Ok(())
    }

    pub(super) fn collect_session(
        &mut self,
        key: crate::sessions::SessionKey,
        now: UnixMillis,
        turn: &mut Turn,
    ) {
        let collection = self.sessions.collect(key, |flow| {
            self.generations
                .ledger(&flow.destination())
                .and_then(|ledger| ledger.snapshot(flow))
                .map(|snapshot| snapshot.phase)
        });
        if collection == super::sessions::Collection::Released
            && self
                .generations
                .connection(&key.receiver())
                .is_some_and(|entry| entry.phase() != super::ConnectionPhase::Closing)
        {
            self.close(key.receiver(), turn);
            self.trace(Some(key.receiver()), RelayDecision::Closing, now);
        }
    }
}
