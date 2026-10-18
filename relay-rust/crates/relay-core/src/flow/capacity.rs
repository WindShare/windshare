//! A peer's immutable receive bound, independent of host scheduling policy.

use super::{ConnectionLedger, Counts, Decision, Input, Observer, Refusal};
use crate::{
    clock::UnixMillis,
    wire::{FrameError, Reader, Writer},
};

/// Positive, independent bounds on outstanding frames and full encoded WS2O bytes.
/// Capacity is not a batch of envelopes, so no frame/byte ratio applies.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ReceiveCapacity(Counts);

pub(super) struct CapacityPools {
    pub(super) startup: Counts,
    pub(super) regular: Counts,
}

impl ReceiveCapacity {
    pub const ENCODED_BYTES: usize = 16;

    #[must_use]
    pub const fn new(frames: u64, encoded_bytes: u64) -> Option<Self> {
        if frames == 0 || encoded_bytes == 0 {
            None
        } else {
            Some(Self(Counts::new(frames, encoded_bytes)))
        }
    }

    #[must_use]
    pub const fn counts(self) -> Counts {
        self.0
    }

    pub(crate) fn read(body: &mut Reader<'_>) -> Result<Self, FrameError> {
        Self::new(
            u64::from_be_bytes(body.array()?),
            u64::from_be_bytes(body.array()?),
        )
        .ok_or(FrameError::Field)
    }

    pub(crate) fn put<const N: usize>(self, out: &mut Writer<N>) {
        out.put(&self.0.frames.to_be_bytes())
            .put(&self.0.encoded_bytes.to_be_bytes());
    }
}

impl<O: Observer> ConnectionLedger<O> {
    /// Binds the first handshake declaration to this physical connection lifetime.
    /// Exact replay is harmless; neither replay nor a later handshake can reset usage.
    ///
    /// # Errors
    /// Rejects a changed declaration or a connection that is already closing.
    pub fn declare_capacity(
        &mut self,
        capacity: ReceiveCapacity,
        now: UnixMillis,
    ) -> Result<(), Refusal> {
        let input = Input::DeclareCapacity(capacity);
        self.run(input, now, |ledger| {
            ledger.ensure_open()?;
            match ledger.receive_capacity {
                Some(current) if current != capacity => return Err(Refusal::CapacityChanged),
                Some(_) => return Ok(()),
                None => {
                    ledger.receive_capacity = Some(capacity);
                    ledger.checkpoint_changed = true;
                }
            }
            ledger.record(
                input,
                Decision::CapacityDeclared {
                    capacity,
                    effective: capacity.counts().min(ledger.limits.total),
                },
                now,
            );
            Ok(())
        })
    }

    #[must_use]
    pub const fn receive_capacity(&self) -> Option<ReceiveCapacity> {
        self.receive_capacity
    }

    /// Additional ordinary credit needed to maintain a host-selected outstanding
    /// window. This is an advisory view, never a reservation or forwarding permit.
    #[must_use]
    pub fn window_credit(&self, flow: super::FlowKey, target: Counts) -> Option<Counts> {
        self.ensure_open().ok()?;
        let entry = self.delivery(flow).ok()?;
        if entry.phase() != super::Phase::Open {
            return None;
        }
        let occupied = entry.usage().total();
        let missing = Counts::new(
            target.frames.saturating_sub(occupied.frames),
            target.encoded_bytes.saturating_sub(occupied.encoded_bytes),
        );
        Some(
            missing
                .min(self.receive_available().ok()?)
                .min(self.capacity_pools().regular.sub(self.regular_used())),
        )
    }

    pub(super) fn receive_available(&self) -> Result<Counts, Refusal> {
        let capacity = self.receive_capacity.ok_or(Refusal::CapacityUndeclared)?;
        Ok(capacity
            .counts()
            .min(self.limits.total)
            .sub(self.usage.total()))
    }

    pub(super) const fn capacity_pools(&self) -> CapacityPools {
        let total = match self.receive_capacity {
            Some(capacity) => self.limits.total.min(capacity.counts()),
            None => self.limits.total,
        };
        // Protect admission inside the declared window too. A small peer
        // must still retain room for ordinary replenishment after startup;
        // reserving its entire window would deadlock its very first flow.
        let startup = self.limits.startup_reserve.min(Counts::new(
            total
                .frames
                .saturating_sub(self.limits.startup_grant.frames),
            total
                .encoded_bytes
                .saturating_sub(self.limits.startup_grant.encoded_bytes),
        ));
        CapacityPools {
            startup,
            regular: total.sub(startup),
        }
    }
}
