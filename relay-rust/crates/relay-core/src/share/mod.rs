//! Shared, no-I/O orchestration of one share. Hosts own sockets, payloads and
//! scheduling; this module owns cross-domain ordering and exact callback identity.
//!
//! `handle` emits storage/send/cleanup commands. Storage completions return as
//! inputs. `submit` is deliberately synchronous: revalidation, charging and host
//! handoff occur under the same exclusive borrow, without an await or cached grant.

use alloc::boxed::Box;

mod checkpoint;
mod checkpoint_codec;
mod checkpoint_tracking;
mod connections;
mod deadlines;
mod delivery;
mod handshake_checkpoint;
mod handshake_restore;
mod lifecycle;
mod restoration;
mod sessions;
mod storage;
mod types;

#[cfg(test)]
#[path = "share_tests.rs"]
mod tests;

pub use checkpoint::{Checkpoint, CheckpointBudget, ConnectionCheckpoint};
pub use checkpoint_tracking::{CheckpointFlush, CheckpointWriteError};
pub use restoration::Restore;
pub use types::*;

use crate::{
    flow::ConnectionLedger,
    handshake::{Admission, ControlHandshake},
    identifiers::RelaySessionId,
    lifecycle::{ConnectionRef, OperationId, Record, ShareLifecycle},
    observation::{Journal, Observation, Observer, RelayDecision},
    sessions::Session,
};
use alloc::collections::{BTreeMap, VecDeque};
use deadlines::{Deadlines, Key};
use storage::{StopRetirement, Task};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Readiness {
    Hidden,
    Ready,
    ReloadRequired,
}

#[derive(Debug)]
enum Role {
    Handshake {
        handshake: Box<ControlHandshake>,
        transcript: handshake_checkpoint::HandshakeCheckpoint,
    },
    Admitting {
        handshake: Box<ControlHandshake>,
        admission: Box<Admission>,
        transcript: handshake_checkpoint::HandshakeCheckpoint,
    },
    Established,
    Closing,
}

#[derive(Debug)]
struct Connection {
    role: Role,
    reservation: Option<OperationId>,
    ledger: ConnectionLedger<Journal>,
}

#[derive(Debug)]
struct AdmissionState {
    session: Session,
    flows: [crate::flow::FlowKey; 2],
    deferred: bool,
}

/// One owner of every control authority. Internal state machines remain independently testable.
#[derive(Debug)]
pub struct ShareRelay<O> {
    identity: Identity,
    limits: Limits,
    lifecycle: ShareLifecycle<Journal>,
    connections: BTreeMap<ConnectionRef, Connection>,
    sessions: BTreeMap<RelaySessionId, AdmissionState>,
    routes: BTreeMap<(ConnectionRef, RelaySessionId), crate::flow::FlowKey>,
    deadlines: Deadlines,
    stored: Option<Record>,
    readiness: Readiness,
    last_session: u64,
    last_work: u64,
    tasks: VecDeque<Task>,
    stop_retirement: Option<StopRetirement>,
    checkpointing: checkpoint_tracking::Checkpointing,
    observer: O,
}

impl<O: Observer> ShareRelay<O> {
    /// The caller injects time and serializes control inputs; no runtime is selected here.
    ///
    /// # Errors
    /// Refuses unknown participants, invalid capacity, stale routes and accounting violations.
    pub fn handle(&mut self, input: Input<'_>, now: crate::clock::UnixMillis) -> Turn {
        let mut turn = Turn::default();
        self.expire(now, &mut turn);
        let result = self.apply(input, now, &mut turn);
        self.flush_lifecycle();
        self.progress_stop(now, &mut turn);
        self.start_work(now, &mut turn);
        turn.deadline = self.deadlines.next();
        // Effects discovered by expiration must never disappear behind a refused input.
        if let Err(error) = result {
            turn.refusal = Some(error);
            self.trace(None, RelayDecision::Rejected(error), now);
        }
        turn
    }

    fn apply(
        &mut self,
        input: Input<'_>,
        now: crate::clock::UnixMillis,
        turn: &mut Turn,
    ) -> Result<(), Error> {
        match input {
            Input::Connected(connection) => {
                let result = self.connect(connection, now);
                if result.is_err() && !self.connections.contains_key(&connection) {
                    turn.outputs.push(Output::Close(connection));
                }
                result
            }
            Input::Control { connection, frame } => self.control(connection, frame, now, turn),
            Input::Challenge { request, seed } => {
                if request.incarnation != self.identity.incarnation {
                    return Err(Error::Unavailable);
                }
                self.challenge(request.connection, seed, now, turn)
            }
            Input::Frame { source, header } => self.receive(source, header, now, turn),
            Input::Admitted { sender, frame } => self.admit_session(sender, frame, now, turn),
            Input::Cancel {
                participant,
                session,
            } => self.cancel_session(participant, session, now, turn),
            Input::Grant { flow, credit } => self.grant(flow, credit, now, turn),
            Input::Consumed {
                connection,
                session,
                total,
            } => self.feedback(connection, session, total, false, now, turn),
            Input::Settled {
                connection,
                session,
                total,
            } => self.feedback(connection, session, total, true, now, turn),
            Input::Discarded(flow) => self.discarded(flow, now, turn),
            Input::Disconnected(connection) => {
                self.disconnect(connection, now, turn);
                Ok(())
            }
            Input::Closed(connection) => {
                self.closed(connection, now, turn);
                Ok(())
            }
            Input::Completed { id, result } => {
                self.complete(id, result, now, turn);
                Ok(())
            }
            Input::Retry(id) => {
                self.retry(id, now, turn);
                Ok(())
            }
            Input::Tick => Ok(()),
        }
    }

    #[must_use]
    pub fn deadline(&self) -> Option<crate::clock::UnixMillis> {
        self.deadlines.next()
    }

    #[must_use]
    pub const fn observer(&self) -> &O {
        &self.observer
    }

    fn flush_lifecycle(&mut self) {
        for item in self.lifecycle.observer_mut().0.drain(..) {
            self.observer.observe(item);
        }
        self.deadlines
            .set(Key::Lifecycle, self.lifecycle.deadline());
    }

    fn flush_ledger(
        &mut self,
        connection: ConnectionRef,
        now: crate::clock::UnixMillis,
        turn: &mut Turn,
    ) {
        if let Some(entry) = self.connections.get_mut(&connection) {
            if self.checkpointing.enabled() && entry.ledger.take_checkpoint_change() {
                self.checkpointing.changed(connection);
            }
            for item in entry.ledger.observer_mut().0.drain(..) {
                self.observer.observe(item);
            }
            self.deadlines
                .set(Key::Ledger(connection), entry.ledger.deadline());
            if entry.ledger.close_request().is_some() && !self.cleanup_deferred() {
                self.disconnect(connection, now, turn);
            }
        }
    }

    fn trace(
        &mut self,
        connection: Option<ConnectionRef>,
        decision: RelayDecision,
        at: crate::clock::UnixMillis,
    ) {
        self.observer.observe(Observation::Relay {
            at,
            relay: self.identity.relay,
            share: self.identity.share,
            incarnation: self.identity.incarnation,
            connection,
            decision,
        });
    }

    fn send(turn: &mut Turn, connection: ConnectionRef, control: Control) {
        turn.outputs.push(Output::Send {
            connection,
            control,
        });
    }

    fn expire(&mut self, now: crate::clock::UnixMillis, turn: &mut Turn) {
        while let Some(key) = self.deadlines.expired(now) {
            match key {
                Key::Lifecycle => {
                    self.lifecycle_event(crate::lifecycle::Event::Tick, None, now, turn);
                }
                Key::Handshake(connection) => self.disconnect(connection, now, turn),
                Key::Session(id) => {
                    self.session_event(id, crate::sessions::Event::Tick, now, turn);
                }
                Key::Ledger(connection) => {
                    if let Some(entry) = self.connections.get_mut(&connection) {
                        let _ = entry.ledger.tick(now);
                    }
                    self.flush_ledger(connection, now, turn);
                }
            }
        }
    }
}
