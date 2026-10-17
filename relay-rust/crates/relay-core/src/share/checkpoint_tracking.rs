//! Opt-in attachment writes: native hosts allocate no change journal.
use super::{CheckpointBudget, ConnectionRef, Error, Observer, RelayDecision, ShareRelay};
use crate::{clock::UnixMillis, flow::Counts};
use alloc::{collections::BTreeSet, vec::Vec};

#[derive(Debug, Default)]
pub(super) enum Checkpointing {
    #[default]
    Disabled,
    Enabled {
        budget: CheckpointBudget,
        dirty: BTreeSet<ConnectionRef>,
    },
}

impl Checkpointing {
    pub(super) const fn enabled(&self) -> bool {
        matches!(self, Self::Enabled { .. })
    }

    pub(super) fn changed(&mut self, connection: ConnectionRef) {
        if let Self::Enabled { dirty, .. } = self {
            dirty.insert(connection);
        }
    }

    pub(super) fn removed(&mut self, connection: ConnectionRef) {
        if let Self::Enabled { dirty, .. } = self {
            dirty.remove(&connection);
        }
    }

    pub(super) fn header_changed(&mut self, connections: impl Iterator<Item = ConnectionRef>) {
        if let Self::Enabled { dirty, .. } = self {
            dirty.extend(connections);
        }
    }
}

/// A failed flush never acknowledges any of the pending connection updates.
#[derive(Debug, Eq, PartialEq)]
pub enum CheckpointWriteError<E> {
    State(Error),
    Write { connection: ConnectionRef, error: E },
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct CheckpointFlush {
    pub connections: usize,
    pub encoded_bytes: usize,
}

impl<O: Observer> ShareRelay<O> {
    /// Enable attachment tracking after construction/restoration and before handling host events.
    /// The initial flush replaces every surviving attachment with the new runtime header.
    ///
    /// # Errors
    /// The complete configured state must fit, including retained retired flows.
    pub fn enable_checkpoints(
        &mut self,
        budget: CheckpointBudget,
        now: UnixMillis,
    ) -> Result<(), Error> {
        if let Err(error) = budget.check(self.limits.sessions) {
            self.trace(None, RelayDecision::Rejected(error), now);
            return Err(error);
        }
        // Native ledgers leave their change bit latched. The initial full flush
        // establishes a new baseline without imposing comparisons on native forwarding.
        for entry in self.connections.values_mut() {
            let _ = entry.ledger.take_checkpoint_change();
        }
        self.checkpointing = Checkpointing::Enabled {
            budget,
            dirty: self.connections.keys().copied().collect(),
        };
        self.trace(
            None,
            RelayDecision::CheckpointingEnabled {
                payload_bytes: budget.payload_bytes(),
            },
            now,
        );
        Ok(())
    }

    /// Synchronously replace changed attachments after executing the event's outputs.
    ///
    /// No callback runs until every pending image is ready. The exclusive borrow
    /// prevents mutation between capture and acknowledgement. A host error retains
    /// the entire batch for idempotent retry, even if earlier writes succeeded.
    ///
    /// # Errors
    /// On any error the host must stay active and retry, or close the affected sockets
    /// before ending the event. Returning with stale live attachments is not safe.
    /// Host wrappers must also enforce their actual structured-serialization limit.
    pub fn flush_checkpoints<E>(
        &mut self,
        now: UnixMillis,
        mut write: impl FnMut(ConnectionRef, &[u8]) -> Result<(), E>,
    ) -> Result<CheckpointFlush, CheckpointWriteError<E>> {
        let pending = match self.prepare_checkpoints() {
            Ok(pending) => pending,
            Err(error) => {
                self.trace(None, RelayDecision::Rejected(error), now);
                return Err(CheckpointWriteError::State(error));
            }
        };
        let mut flushed = CheckpointFlush::default();
        for (connection, bytes) in &pending {
            if let Err(error) = write(*connection, bytes) {
                self.trace(Some(*connection), RelayDecision::CheckpointWriteFailed, now);
                return Err(CheckpointWriteError::Write {
                    connection: *connection,
                    error,
                });
            }
            flushed.connections += 1;
            flushed.encoded_bytes += bytes.len();
        }
        if let Checkpointing::Enabled { dirty, .. } = &mut self.checkpointing {
            dirty.clear();
        }
        if flushed.connections != 0 {
            self.trace(
                None,
                RelayDecision::CheckpointsFlushed {
                    connections: flushed.connections,
                    encoded_bytes: flushed.encoded_bytes,
                },
                now,
            );
        }
        Ok(flushed)
    }

    fn prepare_checkpoints(&self) -> Result<Vec<(ConnectionRef, Vec<u8>)>, Error> {
        self.checkpoint_ready()?;
        let Checkpointing::Enabled { budget, dirty } = &self.checkpointing else {
            return Err(Error::CheckpointDisabled);
        };
        // Clean ledgers have not changed since a successful flush, so only dirty
        // connections can contain payloads that would make this event unsafe to end.
        for connection in dirty {
            let entry = self
                .connections
                .get(connection)
                .ok_or(Error::UnknownConnection)?;
            if entry.ledger.usage().received != Counts::ZERO {
                return Err(Error::CheckpointBusy);
            }
        }
        dirty
            .iter()
            .map(|&connection| {
                let bytes = self.checkpoint_connection(connection)?.encode();
                if bytes.len() > budget.payload_bytes() {
                    return Err(Error::CheckpointCapacity);
                }
                Ok((connection, bytes))
            })
            .collect()
    }
}
