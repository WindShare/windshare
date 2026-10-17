//! Lazily backed slots keep handles stable without charging idle connections for fanout.

use alloc::vec::Vec;

use super::{ConfigurationError, Refusal, delivery::Delivery};

/// Metadata occupancy is independent of delivery credit and payload bytes.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct MetadataUsage {
    /// Includes retired flows awaiting discard, settlement or physical closure.
    pub retained_flows: usize,
    /// Backing storage retained for slot reuse; zero before the first flow.
    pub allocated_slots: usize,
    /// Host-selected hard bound on slot indices and simultaneous retained flows.
    pub limit: usize,
}

#[derive(Debug)]
pub(super) struct DeliverySlots {
    entries: Vec<Option<Delivery>>,
    retained: usize,
    limit: usize,
}

impl DeliverySlots {
    pub(super) const fn new(limit: usize) -> Result<Self, ConfigurationError> {
        if limit == 0 {
            return Err(ConfigurationError::EmptyStorage);
        }
        Ok(Self {
            entries: Vec::new(),
            retained: 0,
            limit,
        })
    }

    pub(super) const fn usage(&self) -> MetadataUsage {
        MetadataUsage {
            retained_flows: self.retained,
            allocated_slots: self.entries.capacity(),
            limit: self.limit,
        }
    }

    pub(super) fn get(&self, slot: usize) -> Option<Delivery> {
        self.entries.get(slot).copied().flatten()
    }

    pub(super) fn iter(&self) -> impl Iterator<Item = &Delivery> {
        self.entries.iter().flatten()
    }

    pub(super) fn vacant(&mut self) -> Result<usize, Refusal> {
        if let Some(slot) = self.entries.iter().position(Option::is_none) {
            return Ok(slot);
        }
        let slot = self.entries.len();
        self.ensure_slot(slot)?;
        Ok(slot)
    }

    /// Recovery can contain holes; allocating the prefix preserves every issued index.
    pub(super) fn ensure_slot(&mut self, slot: usize) -> Result<(), Refusal> {
        if slot >= self.limit {
            return Err(Refusal::FlowCapacity);
        }
        let length = slot + 1;
        if length > self.entries.capacity() {
            // Start at one for the common receiver case. Geometric growth happens
            // only on admission/recovery, never on forwarding or credit feedback.
            let capacity = self
                .entries
                .capacity()
                .saturating_mul(2)
                .max(length)
                .min(self.limit);
            self.entries
                .try_reserve_exact(capacity - self.entries.len())
                .map_err(|_| Refusal::FlowAllocation)?;
        }
        if length > self.entries.len() {
            self.entries.resize(length, None);
        }
        Ok(())
    }

    pub(super) fn replace(&mut self, slot: usize, next: Option<Delivery>) {
        let previous = self.entries[slot];
        self.retained =
            self.retained - usize::from(previous.is_some()) + usize::from(next.is_some());
        self.entries[slot] = next;
        if next.is_none() {
            // Trimming only the tail cannot retarget a surviving handle. Keep the
            // backing allocation while flows remain to avoid settlement churn.
            while self.entries.last().is_some_and(Option::is_none) {
                self.entries.pop();
            }
            if self.retained == 0 {
                self.clear();
            }
        }
    }

    pub(super) fn clear(&mut self) {
        self.entries = Vec::new();
        self.retained = 0;
    }
}
