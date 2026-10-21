use super::{
    Admission, ConnectionRef, Control, Observer, RelayDecision, RelayStep, ShareRelay,
    storage::{AfterWrite, TaskKind},
};
use crate::{
    clock::UnixMillis,
    lifecycle::{Action, Event, Expiration, Phase, StopCleanup, StopWork},
    sessions::Event as SessionEvent,
};
use alloc::{boxed::Box, vec::Vec};

impl<O: Observer> ShareRelay<O> {
    pub(super) fn lifecycle_event(
        &mut self,
        event: Event<'_>,
        requester: Option<ConnectionRef>,
        now: UnixMillis,
        step: &mut RelayStep,
    ) {
        let effects = self.lifecycle.handle(event, now);
        self.flush_lifecycle();
        if let Some(revocation) = effects.revocation {
            let ids: Vec<_> = self.sessions.entries().keys().copied().collect();
            for id in ids {
                self.session_event(id, SessionEvent::Revoke(revocation), now, step);
            }
        }
        if let Some(expiration) = effects.expiration {
            match expiration {
                Expiration::Detached(route) => {
                    self.close(route.lost().connection(), step);
                    self.action(Action::Detached(route), None, now, step);
                }
                Expiration::Released(id) => self.close(id.connection(), step),
                Expiration::Lapsed(_) => self.enqueue(
                    TaskKind::Write {
                        next: None,
                        after: AfterWrite::None,
                    },
                    step,
                ),
            }
        }
        self.action(effects.action, requester, now, step);
    }

    pub(super) fn action(
        &mut self,
        action: Action<'_>,
        requester: Option<ConnectionRef>,
        now: UnixMillis,
        step: &mut RelayStep,
    ) {
        match action {
            Action::Reserved(reservation) => {
                let connection = reservation.id().connection();
                self.offer_admission(connection, Admission::Fresh(reservation), step);
            }
            Action::ResumeAdmitted(attempt) => {
                self.offer_admission(attempt.id().connection(), Admission::Resume(attempt), step);
            }
            Action::Register(request) => {
                self.enqueue(
                    TaskKind::Register {
                        ownership: request.ownership,
                        descriptor: request.descriptor.to_vec(),
                    },
                    step,
                );
            }
            Action::CommitOwnership(commit) => {
                self.enqueue(
                    TaskKind::Write {
                        next: Some(commit.record()),
                        after: AfterWrite::Ownership {
                            id: commit.id(),
                            replaced: commit.replaced(),
                        },
                    },
                    step,
                );
            }
            Action::Registered(registration) => {
                let connection = registration.owner.connection();
                if self.generations.registration_committed(connection) {
                    Self::send(
                        step,
                        connection,
                        Control::Registered(registration.acknowledgement()),
                    );
                }
            }
            Action::Detached(route) => {
                self.enqueue(
                    TaskKind::Write {
                        next: Some(route.record()),
                        after: AfterWrite::None,
                    },
                    step,
                );
            }
            Action::Stop(work) => self.stop_work(work, now, step),
            Action::Stopped(ack) => {
                if self.generations.connection(&ack.connection()).is_some() {
                    Self::send(
                        step,
                        ack.connection(),
                        Control::Stopped(ack.acknowledgement()),
                    );
                    self.trace(
                        Some(ack.connection()),
                        RelayDecision::StopAcknowledged {
                            stop_id: ack.acknowledgement().stop_id(),
                            unsettled_generations: self.generations.unsettled_count(),
                        },
                        now,
                    );
                    // STOP has no delivery capacity or continuing control role.
                    // Closing after the response also makes its recovery record
                    // terminal instead of masquerading as a data participant.
                    self.close(ack.connection(), step);
                }
            }
            Action::Joined(joined) => self.install_session(joined, now, step),
            Action::Released(id) => self.close(id.connection(), step),
            Action::Refused(reason) => {
                if let Some(connection) = requester {
                    if let Some(frame) = reason.frame() {
                        Self::send(step, connection, Control::Error(frame));
                    }
                    self.disconnect(connection, now, step);
                }
            }
            Action::JoinReady(_) | Action::None => {}
        }
    }

    fn stop_work(&mut self, work: StopWork, now: UnixMillis, step: &mut RelayStep) {
        match work {
            StopWork::Persist(pending) => self.enqueue(
                TaskKind::Write {
                    next: Some(pending.record()),
                    after: AfterWrite::Stop(Box::new(work)),
                },
                step,
            ),
            StopWork::Commit(pending) => self.enqueue(
                TaskKind::Write {
                    next: Some(pending.stopped().record()),
                    after: AfterWrite::Stop(Box::new(work)),
                },
                step,
            ),
            StopWork::Retire(pending) => {
                let mut connections = Vec::new();
                match pending.cleanup() {
                    StopCleanup::None => {}
                    StopCleanup::Owner(connection) => {
                        connections.push(connection);
                        for ledger in self.generations.ledgers() {
                            connections.extend(ledger.checkpoint_flows().filter_map(
                                |(flow, _)| {
                                    (flow.session().sender() == connection)
                                        .then_some(flow.session().receiver())
                                },
                            ));
                        }
                    }
                    StopCleanup::Reservation(id) => {
                        connections.push(id.connection());
                    }
                }
                let deferred: Vec<_> = self
                    .sessions
                    .entries()
                    .values()
                    .filter(|entry| entry.deferred)
                    .map(|entry| entry.session.key().wire_id())
                    .collect();
                for id in deferred {
                    if let Some(entry) = self.sessions.entries().get(&id) {
                        connections.push(entry.session.key().receiver());
                    }
                    self.release_cleanup(id, now, step);
                }
                connections.sort_unstable();
                connections.dedup();
                for &connection in &connections {
                    self.close(connection, step);
                }
                // The serialized storage queue confirmed every earlier ownership
                // write before persisting STOP. Only queue discard remains here.
                self.stop_barrier = Some(work);
            }
        }
    }

    pub(super) fn cleanup_deferred(&self) -> bool {
        self.lifecycle.phase() == Phase::Stopping && self.stop_barrier.is_none()
    }
}
