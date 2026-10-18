//! A connection's complete delivery responsibility, including retired flows.
use super::{
    ConnectionLedger, ConnectionState, Counts, Delivery, FlowKey, Limits, Observer,
    ReceiveCapacity, delivery::State,
};
use crate::{clock::UnixMillis, identifiers::RelayIdentity, lifecycle::ConnectionRef};
use alloc::vec::Vec;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct LedgerCheckpoint {
    pub(super) relay: RelayIdentity,
    pub(super) destination: ConnectionRef,
    pub(super) entries: Vec<Delivery>,
    pub(super) state: ConnectionState,
    pub(super) receive_capacity: Option<ReceiveCapacity>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CheckpointError {
    Format,
    Capacity,
    StorageAllocation,
    Identity,
    Accounting,
    PendingPayload,
}

impl LedgerCheckpoint {
    #[must_use]
    pub const fn destination(&self) -> ConnectionRef {
        self.destination
    }
    pub(crate) fn flows(&self) -> impl Iterator<Item = FlowKey> + '_ {
        self.entries.iter().map(|entry| entry.key)
    }
}

impl<O: Observer> ConnectionLedger<O> {
    // The aggregate drains this after each operation, independently of diagnostic logging.
    pub(crate) const fn take_checkpoint_change(&mut self) -> bool {
        core::mem::replace(&mut self.checkpoint_changed, false)
    }

    /// A hibernating host cannot retain an application payload queue.
    ///
    /// # Errors
    /// Refuses snapshots while accepted, unsubmitted payloads remain.
    pub fn checkpoint(&self) -> Result<LedgerCheckpoint, CheckpointError> {
        if self.usage.received != Counts::ZERO {
            return Err(CheckpointError::PendingPayload);
        }
        let mut entries = Vec::with_capacity(self.metadata_usage().retained_flows);
        entries.extend(self.entries.iter().copied());
        Ok(LedgerCheckpoint {
            relay: self.relay,
            destination: self.destination,
            entries,
            state: self.state,
            receive_capacity: self.receive_capacity,
        })
    }

    /// Restores accounting before any capacity can be issued on a surviving socket.
    /// Shape validation is separate from route authorization; callers retire the
    /// returned unauthorized flows through the same cleanup path as live retirement.
    ///
    /// # Errors
    /// Rejects foreign identity, impossible counters, duplicate routes and reduced capacity.
    pub fn restore(
        relay: RelayIdentity,
        destination: ConnectionRef,
        limits: Limits,
        max_flows: usize,
        checkpoint: &LedgerCheckpoint,
        observer: O,
    ) -> Result<Self, CheckpointError> {
        if checkpoint.relay != relay || checkpoint.destination != destination {
            return Err(CheckpointError::Identity);
        }
        if checkpoint.entries.len() > max_flows {
            return Err(CheckpointError::Capacity);
        }
        let mut ledger = Self::new(relay, destination, limits, max_flows, observer)
            .map_err(|_| CheckpointError::Capacity)?;
        ledger.receive_capacity = checkpoint.receive_capacity;
        let pools = ledger.capacity_pools();
        for (index, entry) in checkpoint.entries.iter().enumerate() {
            let slot = entry.key.slot;
            if slot >= max_flows {
                return Err(CheckpointError::Capacity);
            }
            validate_entry(*entry, relay, destination)?;
            if checkpoint.entries[..index]
                .iter()
                .any(|prior| prior.key.slot >= slot || prior.key.permit == entry.key.permit)
            {
                return Err(CheckpointError::Identity);
            }
            let usage = entry.usage();
            let total = ledger
                .usage
                .total()
                .checked_add(usage.total())
                .ok_or(CheckpointError::Accounting)?;
            let startup = ledger
                .startup_used
                .checked_add(entry.startup_used())
                .ok_or(CheckpointError::Accounting)?;
            if !checkpoint
                .receive_capacity
                .is_some_and(|capacity| capacity.counts().covers(total))
                || !limits.total.covers(total)
                || !pools.startup.covers(startup)
                || !pools.regular.covers(total.sub(startup))
            {
                return Err(CheckpointError::Capacity);
            }
            ledger
                .entries
                .ensure_slot(slot)
                .map_err(|_| CheckpointError::StorageAllocation)?;
            ledger.replace(slot, Some(*entry));
        }
        match checkpoint.state {
            ConnectionState::Closing(request)
                if request.destination != destination
                    || request.flow.destination() != destination
                    || request.flow.session().relay() != relay
                    || request.flow.slot >= max_flows
                    || request.deadline.0 == 0 =>
            {
                return Err(CheckpointError::Identity);
            }
            ConnectionState::Closed if !checkpoint.entries.is_empty() => {
                return Err(CheckpointError::Accounting);
            }
            _ => {}
        }
        // A final receipt may have removed the last flow after closure was requested.
        // That does not rescind the pending close or reopen a confirmed closed lifetime.
        ledger.state = checkpoint.state;
        ledger.checkpoint_changed = false;
        Ok(ledger)
    }

    pub(crate) fn checkpoint_flows(&self) -> impl Iterator<Item = (FlowKey, bool)> + '_ {
        self.entries
            .iter()
            .map(|entry| (entry.key, entry.state == State::Open))
    }
}

fn validate_entry(
    entry: Delivery,
    relay: RelayIdentity,
    destination: ConnectionRef,
) -> Result<(), CheckpointError> {
    if entry.key.session().relay() != relay || entry.key.destination() != destination {
        return Err(CheckpointError::Identity);
    }
    if !entry.granted.covers(entry.received)
        || !entry.received.covers(entry.submitted)
        || !entry.submitted.covers(entry.consumed)
        || !entry.granted.covers(entry.startup)
        || !entry.received.is_envelope_batch()
        || !entry.submitted.is_envelope_batch()
        || !entry.consumed.is_envelope_batch()
        || !entry.submitted.sub(entry.consumed).is_envelope_batch()
    {
        return Err(CheckpointError::Accounting);
    }
    if entry.usage().received != Counts::ZERO {
        return Err(CheckpointError::PendingPayload);
    }
    if matches!(entry.deadline(), Some(UnixMillis(0))) {
        return Err(CheckpointError::Accounting);
    }
    Ok(())
}
