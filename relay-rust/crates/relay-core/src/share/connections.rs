use super::Readiness;
use super::{
    Admission, Connection, ConnectionRef, Control, ControlHandshake, Error, Key, Observer, Output,
    RelayDecision, Role, ShareRelay, Turn,
};
use super::{
    ChallengeRequest,
    handshake_checkpoint::{Binding, HandshakeCheckpoint},
};
use crate::{
    clock::UnixMillis,
    flow::ConnectionLedger,
    handshake::{AdmissionRequest, Outcome, Step},
    lifecycle::{Cancellation, Event},
    observation::Journal,
    registration::RegistrationMode,
    sessions::{EndReason, Event as SessionEvent},
};
use alloc::boxed::Box;
use alloc::vec::Vec;
use core::mem;

impl<O: Observer> ShareRelay<O> {
    pub(super) fn connect(
        &mut self,
        connection: ConnectionRef,
        now: UnixMillis,
    ) -> Result<(), Error> {
        if self.readiness == Readiness::ReloadRequired {
            return Err(Error::Unavailable);
        }
        if self.connections.contains_key(&connection) {
            return Err(Error::UnknownConnection);
        }
        if self.connections.len() >= self.limits.connections {
            self.trace(Some(connection), RelayDecision::Capacity, now);
            return Err(Error::Capacity);
        }
        let handshake = ControlHandshake::open(self.identity.relay, connection, now);
        self.deadlines
            .set(Key::Handshake(connection), handshake.deadline());
        let ledger = ConnectionLedger::new(
            self.identity.relay,
            connection,
            self.limits.delivery,
            self.limits.sessions,
            Journal::default(),
        )
        .map_err(|_| Error::Configuration)?;
        self.connections.insert(
            connection,
            Connection {
                role: Role::Handshake {
                    handshake: Box::new(handshake),
                    transcript: HandshakeCheckpoint::new(now),
                },
                reservation: None,
                ledger,
            },
        );
        self.checkpointing.changed(connection);
        self.trace(Some(connection), RelayDecision::Connected, now);
        Ok(())
    }

    pub(super) fn control(
        &mut self,
        connection: ConnectionRef,
        frame: &[u8],
        now: UnixMillis,
        turn: &mut Turn,
    ) -> Result<(), Error> {
        if self.readiness == Readiness::ReloadRequired {
            return Err(Error::Unavailable);
        }
        let entry = self
            .connections
            .get_mut(&connection)
            .ok_or(Error::UnknownConnection)?;
        if matches!(entry.role, Role::Established | Role::Closing) {
            return self.delivery_control(connection, frame, now, turn);
        }
        let role = mem::replace(&mut entry.role, Role::Established);
        let Role::Handshake {
            mut handshake,
            mut transcript,
        } = role
        else {
            entry.role = role;
            self.disconnect(connection, now, turn);
            return Err(Error::Unavailable);
        };
        self.checkpointing.changed(connection);
        let original_deadline = handshake.deadline();
        let step = handshake.receive(frame, now);
        if handshake.phase() != crate::handshake::Phase::Finished {
            transcript.frame(now, frame)?;
        }
        let deadline = if matches!(
            step,
            Step::Complete(Outcome::Publish(_) | Outcome::Resume(_))
        ) {
            original_deadline
        } else {
            handshake.deadline()
        };
        self.deadlines.set(Key::Handshake(connection), deadline);
        entry.role = Role::Handshake {
            handshake,
            transcript,
        };
        self.handshake_step(connection, step, now, turn);
        Ok(())
    }

    pub(super) fn challenge(
        &mut self,
        connection: ConnectionRef,
        seed: crate::challenge::ChallengeSeed,
        now: UnixMillis,
        turn: &mut Turn,
    ) -> Result<(), Error> {
        let entry = self
            .connections
            .get_mut(&connection)
            .ok_or(Error::UnknownConnection)?;
        let role = mem::replace(&mut entry.role, Role::Established);
        let Role::Admitting {
            mut handshake,
            admission,
            mut transcript,
        } = role
        else {
            entry.role = role;
            return Err(Error::Unavailable);
        };
        self.checkpointing.changed(connection);
        transcript.challenge(now, seed.checkpoint())?;
        let step = handshake.admit(*admission, seed, now);
        self.deadlines
            .set(Key::Handshake(connection), handshake.deadline());
        entry.role = Role::Handshake {
            handshake,
            transcript,
        };
        self.handshake_step(connection, step, now, turn);
        Ok(())
    }

    pub(super) fn offer_admission(
        &mut self,
        connection: ConnectionRef,
        admission: Admission,
        turn: &mut Turn,
    ) {
        let Some(entry) = self.connections.get_mut(&connection) else {
            return;
        };
        let Role::Handshake {
            handshake,
            mut transcript,
        } = mem::replace(&mut entry.role, Role::Established)
        else {
            return;
        };
        if matches!(admission, Admission::Resume(_)) {
            transcript.binding = Binding::Resume(super::handshake_checkpoint::ownership(
                self.lifecycle.ownership_record(),
            ));
        }
        entry.role = Role::Admitting {
            handshake,
            admission: Box::new(admission),
            transcript,
        };
        self.checkpointing.changed(connection);
        turn.outputs.push(Output::ChallengeNeeded(ChallengeRequest {
            connection,
            incarnation: self.identity.incarnation,
        }));
    }

    fn handshake_step(
        &mut self,
        connection: ConnectionRef,
        step: Step<'_>,
        now: UnixMillis,
        turn: &mut Turn,
    ) {
        match step {
            Step::AwaitFrame => {}
            Step::SendChallenge(bytes) => {
                Self::send(turn, connection, Control::Challenge(bytes.to_vec()));
            }
            Step::RequestAdmission(request) => {
                self.request_admission(connection, request, now, turn);
            }
            Step::Complete(outcome) => {
                if let Some(entry) = self.connections.get_mut(&connection) {
                    entry.role = Role::Established;
                    self.checkpointing.changed(connection);
                }
                if matches!(outcome, Outcome::Join(_) | Outcome::Stop(_)) {
                    self.deadlines.set(Key::Handshake(connection), None);
                }
                match outcome {
                    Outcome::Publish(publication) => self.lifecycle_event(
                        Event::Publish(publication),
                        Some(connection),
                        now,
                        turn,
                    ),
                    Outcome::Resume(resumption) => {
                        self.lifecycle_event(
                            Event::Resume(resumption),
                            Some(connection),
                            now,
                            turn,
                        );
                    }
                    Outcome::Stop(authority) => self.lifecycle_event(
                        Event::Stop {
                            connection,
                            authority,
                        },
                        Some(connection),
                        now,
                        turn,
                    ),
                    Outcome::Join(request) => self.join(request, now, turn),
                }
            }
            Step::Refuse(error) => {
                Self::send(turn, connection, Control::Error(error));
                self.disconnect(connection, now, turn);
            }
            Step::Close(reason) => {
                self.trace(
                    Some(connection),
                    RelayDecision::HandshakeClosed(reason),
                    now,
                );
                self.disconnect(connection, now, turn);
            }
        }
    }

    fn request_admission(
        &mut self,
        connection: ConnectionRef,
        request: AdmissionRequest,
        now: UnixMillis,
        turn: &mut Turn,
    ) {
        let deadline = self
            .connections
            .get(&connection)
            .and_then(|entry| match &entry.role {
                Role::Handshake { handshake, .. } => handshake.deadline(),
                _ => None,
            });
        let Some(deadline) = deadline else {
            self.disconnect(connection, now, turn);
            return;
        };
        match request {
            AdmissionRequest::Registration(init) => {
                if !self.declare_capacity(connection, init.receive_capacity(), now, turn) {
                    return;
                }
                let event = match init.mode() {
                    RegistrationMode::Fresh => Event::Reserve {
                        connection,
                        init,
                        deadline,
                    },
                    RegistrationMode::Resume => Event::BeginResume {
                        connection,
                        init,
                        deadline,
                    },
                };
                self.lifecycle_event(event, Some(connection), now, turn);
            }
            AdmissionRequest::Stop(init) => {
                if init.route().share_id() == self.identity.share {
                    self.offer_admission(connection, Admission::Stop, turn);
                } else {
                    self.disconnect(connection, now, turn);
                }
            }
        }
    }

    pub(super) fn declare_capacity(
        &mut self,
        connection: ConnectionRef,
        capacity: crate::flow::ReceiveCapacity,
        now: UnixMillis,
        turn: &mut Turn,
    ) -> bool {
        let result = self
            .connections
            .get_mut(&connection)
            .ok_or(Error::UnknownConnection)
            .and_then(|entry| {
                entry
                    .ledger
                    .declare_capacity(capacity, now)
                    .map_err(Error::Flow)
            });
        self.flush_ledger(connection, now, turn);
        if let Err(error) = result {
            self.trace(Some(connection), RelayDecision::Rejected(error), now);
            self.disconnect(connection, now, turn);
            return false;
        }
        true
    }

    pub(super) fn close(&mut self, connection: ConnectionRef, turn: &mut Turn) {
        if let Some(entry) = self.connections.get_mut(&connection) {
            if matches!(entry.role, Role::Closing) {
                return;
            }
            entry.role = Role::Closing;
            self.checkpointing.changed(connection);
            self.deadlines.set(Key::Handshake(connection), None);
            turn.outputs.push(Output::Close(connection));
        }
    }

    pub(super) fn disconnect(
        &mut self,
        connection: ConnectionRef,
        now: UnixMillis,
        turn: &mut Turn,
    ) {
        let Some(entry) = self.connections.get(&connection) else {
            return;
        };
        if matches!(entry.role, Role::Closing) {
            return;
        }
        let reservation = entry.reservation;
        self.close(connection, turn);
        if let Some(id) = reservation {
            self.lifecycle_event(
                Event::Cancel {
                    id,
                    reason: Cancellation::Disconnected,
                },
                None,
                now,
                turn,
            );
        }
        self.lifecycle_event(Event::OwnerDisconnected(connection), None, now, turn);
        let receivers: Vec<_> = self
            .sessions
            .values()
            .filter(|entry| entry.session.key().receiver() == connection)
            .map(|entry| entry.session.key())
            .collect();
        for key in receivers {
            self.session_event(
                key.wire_id(),
                SessionEvent::End {
                    session: key,
                    participant: connection,
                    reason: EndReason::ReceiverDisconnected,
                },
                now,
                turn,
            );
        }
        self.trace(Some(connection), RelayDecision::Closing, now);
    }

    pub(super) fn closed(&mut self, connection: ConnectionRef, now: UnixMillis, turn: &mut Turn) {
        self.disconnect(connection, now, turn);
        let affected: Vec<_> = self
            .connections
            .get(&connection)
            .into_iter()
            .flat_map(|entry| {
                entry
                    .ledger
                    .checkpoint_flows()
                    .map(|(flow, _)| flow.session())
            })
            .collect();
        if let Some(entry) = self.connections.get_mut(&connection) {
            let _ = entry.ledger.confirm_closed(connection, now);
            for event in entry.ledger.observer_mut().0.drain(..) {
                self.observer.observe(event);
            }
        }
        self.connections.remove(&connection);
        self.checkpointing.removed(connection);
        self.deadlines.set(Key::Handshake(connection), None);
        self.deadlines.set(Key::Ledger(connection), None);
        for key in affected {
            self.collect_session(key, now, turn);
        }
        self.trace(Some(connection), RelayDecision::Closed, now);
    }
}
