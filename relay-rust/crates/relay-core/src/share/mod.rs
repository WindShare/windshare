//! Shared, no-I/O orchestration of one share. Hosts own sockets, payloads and
//! scheduling; this module owns cross-domain ordering and exact callback identity.
//!
//! Generation and session owners keep their mutation journals and deadline indexes
//! private. This coordinator merges their projections at one checkpoint boundary.
//!
//! `handle` emits storage/send/cleanup commands. Storage completions return as
//! inputs. `submit` is deliberately synchronous: revalidation, charging and host
//! handoff occur under the same exclusive borrow, without an await or cached grant.

mod admission;
pub use admission::{AttemptLimiter, AttemptRate, HandshakeLimits};
mod checkpoint;
mod checkpoint_codec;
mod checkpoint_tracking;
mod connections;
mod deadlines;
mod delivery;
mod generations;
mod handshake_checkpoint;
mod handshake_restore;
mod lifecycle;
mod liveness;
mod responsibility;
mod restoration;
mod retirement;
use generations::{ConnectionPhase, Detached, Generations};
pub use generations::{ConnectionUsage, RetirementPolicy};
mod session_workflow;
mod sessions;
mod storage;
mod types;

#[cfg(test)]
#[path = "share_tests.rs"]
mod tests;

pub use checkpoint::{Checkpoint, CheckpointBudget, ConnectionCheckpoint};
pub use checkpoint_tracking::{CheckpointFlush, CheckpointTarget, CheckpointWriteError};
pub use responsibility::CheckpointManifest;
pub use restoration::{Authority, Responsibility, Restore};
pub use types::*;

use crate::{
    handshake::Admission,
    identifiers::RelaySessionId,
    lifecycle::{ConnectionRef, OperationId, Record, ShareLifecycle},
    observation::{Journal, Observation, Observer, RelayDecision},
    sessions::Session,
};
use alloc::collections::VecDeque;
use deadlines::Key;
use storage::Task;

// Work-ID exhaustion fences this runtime even when its stored ownership is valid.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum RuntimeStatus {
    Running,
    ReloadRequired,
}

/// One owner of every control authority. Internal state machines remain independently testable.
#[derive(Debug)]
pub struct ShareRelay<O> {
    identity: Identity,
    limits: Limits,
    lifecycle: ShareLifecycle<Journal>,
    generations: Generations,
    sessions: sessions::Sessions,
    stored: Option<Record>,
    runtime: RuntimeStatus,
    last_work: u64,
    tasks: VecDeque<Task>,
    stop_barrier: Option<crate::lifecycle::StopWork>,
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
        turn.deadline = self.deadline();
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
                if result.is_err() && self.generations.connection(&connection).is_none() {
                    turn.outputs.push(Output::Close(connection));
                }
                result
            }
            Input::Register(grant) => self.register(grant, now, turn),
            Input::Terminate {
                credential,
                stop_id,
            } => {
                self.lifecycle_event(
                    crate::lifecycle::Event::Terminate {
                        credential,
                        stop_id,
                    },
                    None,
                    now,
                    turn,
                );
                Ok(())
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
                self.detach(connection, now, turn);
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
        self.next_deadline().map(|(at, _)| at)
    }

    #[must_use]
    pub const fn observer(&self) -> &O {
        &self.observer
    }

    fn flush_lifecycle(&mut self) {
        for item in self.lifecycle.observer_mut().0.drain(..) {
            self.observer.observe(item);
        }
    }

    fn apply_delivery_close(
        &mut self,
        connection: ConnectionRef,
        now: crate::clock::UnixMillis,
        turn: &mut Turn,
    ) {
        if self
            .generations
            .ledger(&connection)
            .is_some_and(|ledger| ledger.close_request().is_some())
            && !self.cleanup_deferred()
        {
            self.disconnect(connection, now, turn);
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

    // Each owner indexes its own deadlines. Comparing three heads preserves the
    // original time/key ordering without scanning connections or sessions.
    fn next_deadline(&self) -> Option<(crate::clock::UnixMillis, Key)> {
        [
            self.lifecycle.deadline().map(|at| (at, Key::Lifecycle)),
            self.generations.next_deadline(),
            self.sessions.next_deadline(),
        ]
        .into_iter()
        .flatten()
        .min()
    }

    fn expire(&mut self, now: crate::clock::UnixMillis, turn: &mut Turn) {
        while let Some((_, key)) = self.next_deadline().filter(|(at, _)| *at <= now) {
            match key {
                Key::Lifecycle => {}
                Key::Session(_) => {
                    let _ = self.sessions.expire(now);
                }
                _ => {
                    let _ = self.generations.expire(now);
                }
            }
            match key {
                Key::Lifecycle => {
                    self.lifecycle_event(crate::lifecycle::Event::Tick, None, now, turn);
                }
                Key::Handshake(connection) | Key::Control(connection) => {
                    self.disconnect(connection, now, turn);
                }
                Key::Liveness(connection) => self.expire_liveness(connection, now, turn),
                Key::Session(id) => {
                    self.session_event(id, crate::sessions::Event::Tick, now, turn);
                }
                Key::Retirement(_) => self.progress_retirements(now),
                Key::Ledger(connection) => {
                    let _ = self.generations.update_ledger(
                        connection,
                        |ledger| ledger.tick(now),
                        &mut self.observer,
                    );
                    self.apply_delivery_close(connection, now, turn);
                }
            }
        }
    }
}
