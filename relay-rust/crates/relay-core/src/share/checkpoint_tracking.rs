//! Opt-in recovery writes: native hosts allocate no change journal.
use super::{CheckpointBudget, ConnectionRef, Error, Observer, RelayDecision, ShareRelay};
use crate::{clock::UnixMillis, flow::Counts};
use alloc::{collections::BTreeSet, vec::Vec};

#[derive(Debug, Default)]
pub(super) enum Checkpointing {
    #[default]
    Disabled,
    Enabled {
        budget: CheckpointBudget,
    },
}

/// Owners retain their own pending projections until the whole host batch succeeds.
/// Disabled owners allocate no journal and skip snapshot comparisons.
#[derive(Debug, Default)]
pub(super) enum Changes {
    #[default]
    Disabled,
    Enabled(Pending),
}

#[derive(Debug, Default)]
pub(super) struct Pending {
    pub connections: BTreeSet<ConnectionRef>,
    pub manifest: bool,
}

impl Changes {
    pub fn tracking(connections: impl IntoIterator<Item = ConnectionRef>, manifest: bool) -> Self {
        Self::Enabled(Pending {
            connections: connections.into_iter().collect(),
            manifest,
        })
    }

    pub const fn enabled(&self) -> bool {
        matches!(self, Self::Enabled(_))
    }

    pub const fn pending(&self) -> Option<&Pending> {
        match self {
            Self::Disabled => None,
            Self::Enabled(pending) => Some(pending),
        }
    }

    pub fn acknowledge(&mut self) {
        if let Self::Enabled(pending) = self {
            pending.connections.clear();
            pending.manifest = false;
        }
    }

    pub const fn retention_changed(&mut self) {
        if let Self::Enabled(pending) = self {
            pending.manifest = true;
        }
    }

    pub fn changed(&mut self, connection: ConnectionRef) {
        if let Self::Enabled(pending) = self {
            pending.connections.insert(connection);
        }
    }

    pub fn inserted(&mut self, connection: ConnectionRef) {
        self.changed(connection);
        self.retention_changed();
    }

    pub fn removed(&mut self, connection: ConnectionRef) {
        if let Self::Enabled(pending) = self {
            pending.connections.remove(&connection);
            pending.manifest = true;
        }
    }
}

/// A failed flush never acknowledges any of the pending connection updates.
#[derive(Debug, Eq, PartialEq)]
pub enum CheckpointWriteError<E> {
    State(Error),
    Write { target: CheckpointTarget, error: E },
}

/// Generation records may outlive sockets; the manifest must survive losing all of them.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CheckpointTarget {
    Generation(ConnectionRef),
    Manifest,
}

impl CheckpointTarget {
    const fn connection(self) -> Option<ConnectionRef> {
        match self {
            Self::Generation(connection) => Some(connection),
            Self::Manifest => None,
        }
    }
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct CheckpointFlush {
    pub connections: usize,
    pub encoded_bytes: usize,
    pub manifest: bool,
}

impl<O: Observer> ShareRelay<O> {
    /// Enable recovery tracking after restoration and before handling host events.
    /// The initial flush replaces every retained record and manifest with the new runtime header.
    ///
    /// # Errors
    /// The complete configured state must fit, including retained retired flows.
    pub fn enable_checkpoints(
        &mut self,
        budget: CheckpointBudget,
        now: UnixMillis,
    ) -> Result<(), Error> {
        if let Err(error) = budget.check(self.limits.retained_flows_per_connection) {
            self.trace(None, RelayDecision::Rejected(error), now);
            return Err(error);
        }
        self.generations.enable_checkpoints();
        self.sessions.enable_checkpoints();
        self.checkpointing = Checkpointing::Enabled { budget };
        self.trace(
            None,
            RelayDecision::CheckpointingEnabled {
                payload_bytes: budget.payload_bytes(),
            },
            now,
        );
        Ok(())
    }

    /// Synchronously replace changed generation records, then the inventory if changed.
    ///
    /// No callback runs until every pending image is ready. The exclusive borrow
    /// prevents mutation between capture and acknowledgement. A host error retains
    /// the entire batch for idempotent retry, even if earlier writes succeeded.
    ///
    /// # Errors
    /// Retry a failed batch before ending the event. If a delivery write cannot
    /// be recovered, omit that generation's record instead of replaying stale credit.
    /// The independent manifest and lifecycle facts must remain authoritative;
    /// uncertainty there still refuses recovery. Enforce serialization limits. Removed
    /// generations may be deleted only after the new manifest is committed.
    pub fn flush_checkpoints<E>(
        &mut self,
        now: UnixMillis,
        mut write: impl FnMut(CheckpointTarget, &[u8]) -> Result<(), E>,
    ) -> Result<CheckpointFlush, CheckpointWriteError<E>> {
        let pending = match self.prepare_checkpoints() {
            Ok(pending) => pending,
            Err(error) => {
                self.trace(None, RelayDecision::Rejected(error), now);
                return Err(CheckpointWriteError::State(error));
            }
        };
        let mut flushed = CheckpointFlush::default();
        for (target, bytes) in &pending {
            if let Err(error) = write(*target, bytes) {
                self.trace(
                    target.connection(),
                    RelayDecision::CheckpointWriteFailed,
                    now,
                );
                return Err(CheckpointWriteError::Write {
                    target: *target,
                    error,
                });
            }
            match target {
                CheckpointTarget::Generation(_) => flushed.connections += 1,
                CheckpointTarget::Manifest => flushed.manifest = true,
            }
            flushed.encoded_bytes += bytes.len();
        }
        self.generations.acknowledge_checkpoints();
        self.sessions.acknowledge_checkpoints();
        if !pending.is_empty() {
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

    fn prepare_checkpoints(&self) -> Result<Vec<(CheckpointTarget, Vec<u8>)>, Error> {
        self.checkpoint_ready()?;
        let Checkpointing::Enabled { budget } = &self.checkpointing else {
            return Err(Error::CheckpointDisabled);
        };
        let generations = self
            .generations
            .checkpoint_changes()
            .expect("tracking enabled");
        let sessions = self
            .sessions
            .checkpoint_changes()
            .expect("tracking enabled");
        // Session snapshots belong to receiver records. A generation removed in
        // the same turn must not be resurrected by its session's pending change.
        let dirty = || {
            generations
                .connections
                .union(&sessions.connections)
                .copied()
                .filter(|connection| self.generations.has_record(connection))
        };
        // Clean ledgers have not changed since a successful flush, so only dirty
        // connections can contain payloads that would make this event unsafe to end.
        for connection in dirty() {
            if self.generations.control_deadline(&connection).is_some() {
                continue;
            }
            let ledger = self
                .generations
                .ledger(&connection)
                .ok_or(Error::UnknownConnection)?;
            if ledger.usage().received != Counts::ZERO {
                return Err(Error::CheckpointBusy);
            }
        }
        let mut pending = dirty()
            .map(|connection| {
                let bytes = self.checkpoint_connection(connection)?.encode();
                if bytes.len() > budget.payload_bytes() {
                    return Err(Error::CheckpointCapacity);
                }
                Ok((CheckpointTarget::Generation(connection), bytes))
            })
            .collect::<Result<Vec<_>, _>>()?;
        if generations.manifest || sessions.manifest {
            pending.push((
                CheckpointTarget::Manifest,
                self.checkpoint_manifest()?.encode(),
            ));
        }
        Ok(pending)
    }
}
