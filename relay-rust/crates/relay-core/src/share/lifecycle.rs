use super::{
    Admission, ConnectionRef, Control, Observer, OperationId, RelayDecision, ShareRelay,
    StopBarrier, Turn,
    storage::{AfterWrite, TaskKind},
};
use crate::{
    clock::UnixMillis,
    lifecycle::{Action, Event, Expiration, Phase, StopCleanup, StopWork},
    sessions::Event as SessionEvent,
};
use alloc::vec::Vec;

impl<O: Observer> ShareRelay<O> {
    pub(super) fn lifecycle_event(
        &mut self,
        event: Event<'_>,
        requester: Option<ConnectionRef>,
        now: UnixMillis,
        turn: &mut Turn,
    ) {
        let effects = self.lifecycle.handle(event, now);
        self.flush_lifecycle();
        if let Some(revocation) = effects.revocation {
            let ids: Vec<_> = self.sessions.keys().copied().collect();
            for id in ids {
                self.session_event(id, SessionEvent::Revoke(revocation), now, turn);
            }
        }
        if let Some(expiration) = effects.expiration {
            match expiration {
                Expiration::Detached(route) => {
                    self.close(route.lost().connection(), turn);
                    self.action(Action::Detached(route), None, now, turn);
                }
                Expiration::Released(id) => self.close(id.connection(), turn),
                Expiration::Reconcile(id) => self.reconcile(id, turn),
                Expiration::Lapsed(_) => self.enqueue(
                    TaskKind::Write {
                        next: None,
                        after: AfterWrite::None,
                    },
                    turn,
                ),
            }
        }
        self.action(effects.action, requester, now, turn);
    }

    pub(super) fn action(
        &mut self,
        action: Action<'_>,
        requester: Option<ConnectionRef>,
        now: UnixMillis,
        turn: &mut Turn,
    ) {
        match action {
            Action::Reserved(reservation) => {
                let connection = reservation.id().connection();
                if let Some(entry) = self.connections.get_mut(&connection) {
                    entry.reservation = Some(reservation.id());
                }
                self.offer_admission(connection, Admission::Fresh(reservation), turn);
            }
            Action::ResumeAdmitted(attempt) => {
                self.offer_admission(attempt.id().connection(), Admission::Resume(attempt), turn);
            }
            Action::Publish(request) => {
                self.enqueue(
                    TaskKind::Publish {
                        id: request.id,
                        record: request.record(),
                        descriptor: request.descriptor.to_vec(),
                    },
                    turn,
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
                    turn,
                );
            }
            Action::Registered(registration) => {
                let connection = registration.owner.connection();
                if let Some(entry) = self.connections.get_mut(&connection) {
                    entry.reservation = None;
                    Self::send(
                        turn,
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
                    turn,
                );
            }
            Action::Stop(work) => self.stop_work(work, now, turn),
            Action::Stopped(ack) => {
                if self.connections.contains_key(&ack.connection()) {
                    Self::send(
                        turn,
                        ack.connection(),
                        Control::Stopped(ack.acknowledgement()),
                    );
                    self.trace(
                        Some(ack.connection()),
                        RelayDecision::StopAcknowledged {
                            stop_id: ack.acknowledgement().stop_id(),
                            unsettled_generations: self
                                .ledgers
                                .values()
                                .filter(|ledger| ledger.metadata_usage().retained_flows != 0)
                                .count(),
                        },
                        now,
                    );
                    // STOP has no delivery capacity or continuing control role.
                    // Closing after the response also makes its recovery record
                    // terminal instead of masquerading as a data participant.
                    self.close(ack.connection(), turn);
                }
            }
            Action::Joined(joined) => self.install_session(joined, now, turn),
            Action::Released(id) => self.close(id.connection(), turn),
            Action::Reconcile(id) => self.reconcile(id, turn),
            Action::Refused(reason) => {
                if let Some(connection) = requester {
                    if let Some(frame) = reason.frame() {
                        Self::send(turn, connection, Control::Error(frame));
                    }
                    self.disconnect(connection, now, turn);
                }
            }
            Action::JoinReady(_) | Action::None => {}
        }
    }

    fn reconcile(&mut self, id: OperationId, turn: &mut Turn) {
        self.close(id.connection(), turn);
        self.enqueue(TaskKind::Reconcile(id), turn);
    }

    fn stop_work(&mut self, work: StopWork, now: UnixMillis, turn: &mut Turn) {
        match work {
            StopWork::Persist(pending) => self.enqueue(
                TaskKind::Write {
                    next: Some(pending.record()),
                    after: AfterWrite::Stop(work),
                },
                turn,
            ),
            StopWork::Commit(pending) => self.enqueue(
                TaskKind::Write {
                    next: Some(pending.stopped().record()),
                    after: AfterWrite::Stop(work),
                },
                turn,
            ),
            StopWork::Retire(pending) => {
                let mut connections = Vec::new();
                let operation = match pending.cleanup() {
                    StopCleanup::None => None,
                    StopCleanup::Owner(connection) => {
                        connections.push(connection);
                        for ledger in self.ledgers.values() {
                            connections.extend(ledger.checkpoint_flows().filter_map(
                                |(flow, _)| {
                                    (flow.session().sender() == connection)
                                        .then_some(flow.session().receiver())
                                },
                            ));
                        }
                        None
                    }
                    StopCleanup::Reservation(id) => {
                        connections.push(id.connection());
                        None
                    }
                    StopCleanup::Publication(id) => {
                        connections.push(id.connection());
                        Some(id)
                    }
                };
                let deferred: Vec<_> = self
                    .sessions
                    .values()
                    .filter(|entry| entry.deferred)
                    .map(|entry| entry.session.key().wire_id())
                    .collect();
                for id in deferred {
                    if let Some(entry) = self.sessions.get(&id) {
                        connections.push(entry.session.key().receiver());
                    }
                    self.release_cleanup(id, now, turn);
                }
                connections.sort_unstable();
                connections.dedup();
                for &connection in &connections {
                    self.close(connection, turn);
                }
                self.stop_barrier = Some(StopBarrier {
                    work,
                    publication: operation,
                });
                if let Some(id) = operation {
                    self.enqueue(TaskKind::Quiesce(id), turn);
                }
            }
        }
    }

    pub(super) fn cleanup_deferred(&self) -> bool {
        self.lifecycle.phase() == Phase::Stopping && self.stop_barrier.is_none()
    }
}
