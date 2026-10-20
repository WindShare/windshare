use super::RuntimeStatus;
use super::{
    Admission, ConnectionRef, Control, Error, Key, Observer, Output, RelayDecision, Role,
    ShareRelay, Turn,
};
use super::{ChallengeRequest, handshake_checkpoint::Binding};
use crate::{
    clock::UnixMillis,
    handshake::{AdmissionRequest, Outcome, Step},
    heartbeat::Liveness,
    lifecycle::{Cancellation, Event},
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
        if self.runtime == RuntimeStatus::ReloadRequired {
            return Err(Error::Unavailable);
        }
        if let Err(error) = self.generations.check_control_admission(connection) {
            if error == Error::Capacity {
                self.trace(
                    Some(connection),
                    RelayDecision::ControlCapacity {
                        usage: self.connection_usage(),
                    },
                    now,
                );
            }
            return Err(error);
        }
        let previous = self.handshake_rate.state();
        let accepted = self.handshake_rate.attempt(now);
        if self.handshake_rate.state() != previous {
            self.checkpointing.retention_changed();
        }
        if !accepted {
            self.trace(Some(connection), RelayDecision::HandshakeRateLimited, now);
            return Err(Error::HandshakeRate);
        }
        self.generations.connect(
            connection,
            now,
            &mut self.checkpointing,
            &mut self.deadlines,
        )?;
        self.trace(Some(connection), RelayDecision::Connected, now);
        Ok(())
    }

    pub(super) fn register(
        &mut self,
        grant: crate::access::RegistrationGrant,
        now: UnixMillis,
        turn: &mut Turn,
    ) -> Result<(), Error> {
        let connection = grant.connection();
        let Some((_, init, attribution)) = grant.consume(now) else {
            Self::send(
                turn,
                connection,
                Control::Error(crate::error_frame::ErrorFrame::AccessDenied),
            );
            self.disconnect(connection, now, turn);
            return Err(Error::Unavailable);
        };
        let entry = self
            .generations
            .connection_mut(&connection)
            .ok_or(Error::UnknownConnection)?;
        let Role::Handshake {
            handshake,
            transcript,
        } = &mut entry.role
        else {
            return Err(Error::Unavailable);
        };
        if handshake.phase() != crate::handshake::Phase::AwaitingFirstFrame {
            return Err(Error::Unavailable);
        }
        transcript.binding = Binding::Fresh(attribution);
        self.control(connection, &init.encode(), now, turn)
    }

    pub(super) fn control(
        &mut self,
        connection: ConnectionRef,
        frame: &[u8],
        now: UnixMillis,
        turn: &mut Turn,
    ) -> Result<(), Error> {
        if self.runtime == RuntimeStatus::ReloadRequired {
            return Err(Error::Unavailable);
        }
        let first = self
            .generations
            .connection(&connection)
            .is_some_and(|entry| {
                matches!(
                    &entry.role, Role::Handshake { handshake, .. }
                        if handshake.phase() == crate::handshake::Phase::AwaitingFirstFrame
                )
            });
        if first
            && crate::handshake::Initiation::decode(frame)
                .is_ok_and(|intent| intent.uses_business_capacity())
            && let Err(error) = self.admit_business(connection, now)
        {
            Self::send(
                turn,
                connection,
                Control::Error(crate::error_frame::ErrorFrame::Admission(
                    crate::error_frame::RetryAfter::IMMEDIATE,
                )),
            );
            self.disconnect(connection, now, turn);
            return Err(error);
        }
        let entry = self
            .generations
            .connection_mut(&connection)
            .ok_or(Error::UnknownConnection)?;
        if matches!(
            entry.role,
            Role::Established(_) | Role::Stopping | Role::Closing
        ) {
            return self.connection_control(connection, frame, now, turn);
        }
        let role = mem::replace(&mut entry.role, Role::Closing);
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
        let step = handshake.receive(frame, now);
        if handshake.phase() != crate::handshake::Phase::Finished {
            transcript.frame(now, frame)?;
        }
        self.deadlines
            .set(Key::Handshake(connection), handshake.deadline());
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
            .generations
            .connection_mut(&connection)
            .ok_or(Error::UnknownConnection)?;
        let role = mem::replace(&mut entry.role, Role::Closing);
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
        let Some(entry) = self.generations.connection_mut(&connection) else {
            return;
        };
        let role = mem::replace(&mut entry.role, Role::Closing);
        let Role::Handshake {
            handshake,
            mut transcript,
        } = role
        else {
            entry.role = role;
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
                if let Some(entry) = self.generations.connection_mut(&connection) {
                    if matches!(outcome, Outcome::Stop(_)) {
                        entry.role = Role::Stopping;
                    } else {
                        let liveness = Liveness::established(now);
                        self.deadlines
                            .set(Key::Liveness(connection), Some(liveness.deadline()));
                        entry.role = Role::Established(liveness);
                    }
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
        let deadline =
            self.generations
                .connection(&connection)
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
                    RegistrationMode::Fresh => {
                        let attribution =
                            self.generations
                                .connection(&connection)
                                .and_then(|entry| match &entry.role {
                                    Role::Handshake { transcript, .. } => {
                                        match transcript.binding {
                                            Binding::Fresh(attribution) => Some(attribution),
                                            _ => None,
                                        }
                                    }
                                    _ => None,
                                });
                        let Some(attribution) = attribution else {
                            Self::send(
                                turn,
                                connection,
                                Control::Error(crate::error_frame::ErrorFrame::AccessDenied),
                            );
                            self.disconnect(connection, now, turn);
                            return;
                        };
                        Event::Reserve {
                            connection,
                            init,
                            deadline,
                            attribution,
                        }
                    }
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
            .generations
            .update_ledger(
                connection,
                |ledger| ledger.declare_capacity(capacity, now),
                &mut self.checkpointing,
                &mut self.deadlines,
                &mut self.observer,
            )
            .ok_or(Error::UnknownConnection)
            .and_then(|result| result.map_err(Error::Flow));
        self.apply_delivery_close(connection, now, turn);
        if let Err(error) = result {
            self.trace(Some(connection), RelayDecision::Rejected(error), now);
            self.disconnect(connection, now, turn);
            return false;
        }
        true
    }

    pub(super) fn close(&mut self, connection: ConnectionRef, turn: &mut Turn) {
        if self
            .generations
            .close(connection, &mut self.checkpointing, &mut self.deadlines)
        {
            turn.outputs.push(Output::Close(connection));
        }
    }

    pub(super) fn disconnect(
        &mut self,
        connection: ConnectionRef,
        now: UnixMillis,
        turn: &mut Turn,
    ) {
        let Some(entry) = self.generations.connection(&connection) else {
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

    pub(super) fn detach(&mut self, connection: ConnectionRef, now: UnixMillis, turn: &mut Turn) {
        self.disconnect(connection, now, turn);
        self.generations.detach(
            connection,
            now,
            &mut self.checkpointing,
            &mut self.deadlines,
        );
        self.progress_retirements(now);
        self.trace(Some(connection), RelayDecision::Disconnected, now);
    }

    pub(super) fn collect_ledger(&mut self, connection: ConnectionRef, now: UnixMillis) {
        if self.generation_quiescent(connection)
            && self
                .generations
                .collect(connection, &mut self.checkpointing, &mut self.deadlines)
        {
            self.trace(Some(connection), RelayDecision::ResponsibilityReleased, now);
        }
    }

    pub(super) fn closed(&mut self, connection: ConnectionRef, now: UnixMillis, turn: &mut Turn) {
        self.detach(connection, now, turn);
        let affected = self.generations.release(
            connection,
            now,
            &mut self.checkpointing,
            &mut self.deadlines,
            &mut self.observer,
        );
        for key in affected {
            self.collect_session(key, now, turn);
        }
        self.progress_retirements(now);
        self.trace(Some(connection), RelayDecision::Closed, now);
    }
}
