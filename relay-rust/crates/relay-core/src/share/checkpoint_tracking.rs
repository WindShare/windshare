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
        dirty: BTreeSet<ConnectionRef>,
        manifest_dirty: bool,
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

    pub(super) fn inserted(&mut self, connection: ConnectionRef) {
        if let Self::Enabled {
            dirty,
            manifest_dirty,
            ..
        } = self
        {
            dirty.insert(connection);
            *manifest_dirty = true;
        }
    }

    pub(super) fn removed(&mut self, connection: ConnectionRef) {
        if let Self::Enabled {
            dirty,
            manifest_dirty,
            ..
        } = self
        {
            dirty.remove(&connection);
            *manifest_dirty = true;
        }
    }

    pub(super) fn header_changed(&mut self, connections: impl Iterator<Item = ConnectionRef>) {
        if let Self::Enabled {
            dirty,
            manifest_dirty,
            ..
        } = self
        {
            dirty.extend(connections);
            *manifest_dirty = true;
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
        if let Err(error) = budget.check(self.limits.sessions) {
            self.trace(None, RelayDecision::Rejected(error), now);
            return Err(error);
        }
        // Native ledgers leave their change bit latched. The initial full flush
        // establishes a new baseline without imposing comparisons on native forwarding.
        for ledger in self.ledgers.values_mut() {
            let _ = ledger.take_checkpoint_change();
        }
        self.checkpointing = Checkpointing::Enabled {
            budget,
            dirty: self.ledgers.keys().copied().collect(),
            manifest_dirty: true,
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

    /// Synchronously replace changed generation records, then the inventory if changed.
    ///
    /// No callback runs until every pending image is ready. The exclusive borrow
    /// prevents mutation between capture and acknowledgement. A host error retains
    /// the entire batch for idempotent retry, even if earlier writes succeeded.
    ///
    /// # Errors
    /// On error the host must retain responsibility and retry before ending the
    /// event. Closing sockets does not repair stale accounting. If the latest state
    /// cannot be recovered, restore with unknown responsibility, never old budgets.
    /// Hosts must retain records independently of lost attachments, publish a
    /// coherent recovery state and enforce their serialization limits. Removed
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
        if let Checkpointing::Enabled {
            dirty,
            manifest_dirty,
            ..
        } = &mut self.checkpointing
        {
            dirty.clear();
            *manifest_dirty = false;
        }
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
        let Checkpointing::Enabled {
            budget,
            dirty,
            manifest_dirty,
        } = &self.checkpointing
        else {
            return Err(Error::CheckpointDisabled);
        };
        // Clean ledgers have not changed since a successful flush, so only dirty
        // connections can contain payloads that would make this event unsafe to end.
        for connection in dirty {
            let ledger = self
                .ledgers
                .get(connection)
                .ok_or(Error::UnknownConnection)?;
            if ledger.usage().received != Counts::ZERO {
                return Err(Error::CheckpointBusy);
            }
        }
        let mut pending = dirty
            .iter()
            .map(|&connection| {
                let bytes = self.checkpoint_connection(connection)?.encode();
                if bytes.len() > budget.payload_bytes() {
                    return Err(Error::CheckpointCapacity);
                }
                Ok((CheckpointTarget::Generation(connection), bytes))
            })
            .collect::<Result<Vec<_>, _>>()?;
        if *manifest_dirty {
            pending.push((
                CheckpointTarget::Manifest,
                self.checkpoint_manifest()?.encode(),
            ));
        }
        Ok(pending)
    }
}
