use super::ChallengeRequest;
use super::RuntimeStatus;
use super::generations::Connection;
use super::{
    Admission, ConnectionPhase, ConnectionRef, Control, Error, Observer, Output, RelayDecision,
    RelayStep, ShareRelay,
};
use crate::{
    clock::UnixMillis,
    handshake::{AdmissionRequest, Outcome, Step as HandshakeStep},
    lifecycle::{Cancellation, Event},
    registration::RegistrationMode,
    sessions::{EndReason, Event as SessionEvent},
};
use alloc::vec::Vec;

impl<O: Observer> ShareRelay<O> {
    pub(super) fn connect(
        &mut self,
        connection: ConnectionRef,
        now: UnixMillis,
    ) -> Result<(), Error> {
        if self.runtime == RuntimeStatus::ReloadRequired {
            return Err(Error::Unavailable);
        }
        if let Err(error) = self.generations.connect(connection, now) {
            let decision = match error {
                Error::Capacity => Some(RelayDecision::ControlCapacity {
                    usage: self.connection_usage(),
                }),
                Error::HandshakeRate => Some(RelayDecision::HandshakeRateLimited),
                _ => None,
            };
            if let Some(decision) = decision {
                self.trace(Some(connection), decision, now);
            }
            return Err(error);
        }
        self.trace(Some(connection), RelayDecision::Connected, now);
        Ok(())
    }

    pub(super) fn register(
        &mut self,
        authorization: crate::access::RegistrationAuthorization,
        now: UnixMillis,
        step: &mut RelayStep,
    ) -> Result<(), Error> {
        let connection = authorization.connection();
        let (init, attribution) = match authorization.consume(now) {
            Ok(authorized) => authorized,
            Err(refusal) => {
                self.trace(
                    Some(connection),
                    RelayDecision::RegistrationRefused(refusal),
                    now,
                );
                Self::send(step, connection, Control::Error(refusal));
                self.disconnect(connection, now, step);
                return Err(Error::Unavailable);
            }
        };
        self.generations
            .bind_registration(connection, attribution)?;
        self.trace(
            Some(connection),
            RelayDecision::RegistrationAuthorized(attribution),
            now,
        );
        self.control(connection, &init.encode(), now, step)
    }

    pub(super) fn control(
        &mut self,
        connection: ConnectionRef,
        frame: &[u8],
        now: UnixMillis,
        step: &mut RelayStep,
    ) -> Result<(), Error> {
        if self.runtime == RuntimeStatus::ReloadRequired {
            return Err(Error::Unavailable);
        }
        let phase = self
            .generations
            .connection(&connection)
            .map(Connection::phase);
        let first = phase == Some(ConnectionPhase::AwaitingFirstFrame);
        if first
            && crate::handshake::Initiation::decode(frame)
                .is_ok_and(|intent| intent.uses_business_capacity())
            && let Err(error) = self.admit_business(connection, now)
        {
            Self::send(
                step,
                connection,
                Control::Error(crate::error_frame::ErrorFrame::Admission(
                    crate::error_frame::RetryAfter::IMMEDIATE,
                )),
            );
            self.disconnect(connection, now, step);
            return Err(error);
        }
        if matches!(
            phase,
            Some(
                ConnectionPhase::Established | ConnectionPhase::Stopping | ConnectionPhase::Closing
            )
        ) {
            return self.connection_control(connection, frame, now, step);
        }
        let handshake = match self.generations.receive_handshake(connection, frame, now) {
            Ok(step) => step,
            Err(error) => {
                self.disconnect(connection, now, step);
                return Err(error);
            }
        };
        self.handshake_step(connection, handshake, now, step);
        Ok(())
    }

    pub(super) fn challenge(
        &mut self,
        connection: ConnectionRef,
        seed: crate::challenge::ChallengeSeed,
        now: UnixMillis,
        step: &mut RelayStep,
    ) -> Result<(), Error> {
        let handshake = self
            .generations
            .challenge(connection, seed, now)
            .inspect_err(|error| {
                if *error == Error::InvalidCheckpoint {
                    self.disconnect(connection, now, step);
                }
            })?;
        self.handshake_step(connection, handshake, now, step);
        Ok(())
    }

    pub(super) fn offer_admission(
        &mut self,
        connection: ConnectionRef,
        admission: Admission,
        step: &mut RelayStep,
    ) {
        if !self.generations.offer_admission(
            connection,
            admission,
            self.lifecycle.ownership_record(),
        ) {
            return;
        }
        step.outputs.push(Output::ChallengeNeeded(ChallengeRequest {
            connection,
            incarnation: self.identity.incarnation,
        }));
    }

    fn handshake_step(
        &mut self,
        connection: ConnectionRef,
        handshake: HandshakeStep<'_>,
        now: UnixMillis,
        step: &mut RelayStep,
    ) {
        match handshake {
            HandshakeStep::AwaitFrame => {}
            HandshakeStep::SendChallenge(bytes) => {
                Self::send(step, connection, Control::Challenge(bytes.to_vec()));
            }
            HandshakeStep::RequestAdmission(request) => {
                self.request_admission(connection, request, now, step);
            }
            HandshakeStep::Complete(outcome) => match outcome {
                Outcome::Publish(publication) => {
                    self.lifecycle_event(Event::Publish(publication), Some(connection), now, step);
                }
                Outcome::Resume(resumption) => {
                    self.lifecycle_event(Event::Resume(resumption), Some(connection), now, step);
                }
                Outcome::Stop(authority) => self.lifecycle_event(
                    Event::Stop {
                        connection,
                        authority,
                    },
                    Some(connection),
                    now,
                    step,
                ),
                Outcome::Join(request) => self.join(request, now, step),
            },
            HandshakeStep::Refuse(error) => {
                Self::send(step, connection, Control::Error(error));
                self.disconnect(connection, now, step);
            }
            HandshakeStep::Close(reason) => {
                self.trace(
                    Some(connection),
                    RelayDecision::HandshakeClosed(reason),
                    now,
                );
                self.disconnect(connection, now, step);
            }
        }
    }

    fn request_admission(
        &mut self,
        connection: ConnectionRef,
        request: AdmissionRequest,
        now: UnixMillis,
        step: &mut RelayStep,
    ) {
        let deadline = self
            .generations
            .connection(&connection)
            .and_then(Connection::handshake_deadline);
        let Some(deadline) = deadline else {
            self.disconnect(connection, now, step);
            return;
        };
        match request {
            AdmissionRequest::Registration(init) => {
                if !self.declare_capacity(connection, init.receive_capacity(), now, step) {
                    return;
                }
                let event = match init.mode() {
                    RegistrationMode::Fresh => {
                        let attribution = self
                            .generations
                            .connection(&connection)
                            .and_then(Connection::fresh_attribution);
                        let Some(attribution) = attribution else {
                            Self::send(
                                step,
                                connection,
                                Control::Error(crate::error_frame::ErrorFrame::AccessDenied),
                            );
                            self.disconnect(connection, now, step);
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
                self.lifecycle_event(event, Some(connection), now, step);
            }
            AdmissionRequest::Stop(init) => {
                if init.route().share_id() == self.identity.share {
                    self.offer_admission(connection, Admission::Stop, step);
                } else {
                    self.disconnect(connection, now, step);
                }
            }
        }
    }

    pub(super) fn declare_capacity(
        &mut self,
        connection: ConnectionRef,
        capacity: crate::flow::ReceiveCapacity,
        now: UnixMillis,
        step: &mut RelayStep,
    ) -> bool {
        let result = self
            .generations
            .update_ledger(
                connection,
                |ledger| ledger.declare_capacity(capacity, now),
                &mut self.observer,
            )
            .ok_or(Error::UnknownConnection)
            .and_then(|result| result.map_err(Error::Flow));
        self.apply_delivery_close(connection, now, step);
        if let Err(error) = result {
            self.trace(Some(connection), RelayDecision::Rejected(error), now);
            self.disconnect(connection, now, step);
            return false;
        }
        true
    }

    pub(super) fn close(&mut self, connection: ConnectionRef, step: &mut RelayStep) {
        if self.generations.close(connection) {
            step.outputs.push(Output::Close(connection));
        }
    }

    pub(super) fn disconnect(
        &mut self,
        connection: ConnectionRef,
        now: UnixMillis,
        step: &mut RelayStep,
    ) {
        let Some(entry) = self.generations.connection(&connection) else {
            return;
        };
        if entry.phase() == ConnectionPhase::Closing {
            return;
        }
        let reservation = entry.reservation();
        self.close(connection, step);
        if let Some(id) = reservation {
            self.lifecycle_event(
                Event::Cancel {
                    id,
                    reason: Cancellation::Disconnected,
                },
                None,
                now,
                step,
            );
        }
        self.lifecycle_event(Event::OwnerDisconnected(connection), None, now, step);
        let receivers: Vec<_> = self
            .sessions
            .entries()
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
                step,
            );
        }
        self.trace(Some(connection), RelayDecision::Closing, now);
    }

    pub(super) fn detach(
        &mut self,
        connection: ConnectionRef,
        now: UnixMillis,
        step: &mut RelayStep,
    ) {
        self.disconnect(connection, now, step);
        self.generations.detach(connection, now);
        self.progress_retirements(now);
        self.trace(Some(connection), RelayDecision::Disconnected, now);
    }

    pub(super) fn collect_ledger(&mut self, connection: ConnectionRef, now: UnixMillis) {
        if self.generation_quiescent(connection) && self.generations.collect(connection) {
            self.trace(Some(connection), RelayDecision::ResponsibilityReleased, now);
        }
    }

    pub(super) fn closed(
        &mut self,
        connection: ConnectionRef,
        now: UnixMillis,
        step: &mut RelayStep,
    ) {
        self.detach(connection, now, step);
        let affected = self
            .generations
            .release(connection, now, &mut self.observer);
        for key in affected {
            self.collect_session(key, now, step);
        }
        self.progress_retirements(now);
        self.trace(Some(connection), RelayDecision::Closed, now);
    }
}
