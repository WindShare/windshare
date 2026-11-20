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

/// Host policy for destinations whose capacity many flows share.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct WindowShare {
    /// Outstanding window each flow may always refill to.
    pub floor: Counts,
    /// Flows the destination should be able to admit concurrently.
    pub flows: u64,
}

const fn saturating_sub(left: Counts, right: Counts) -> Counts {
    Counts::new(
        left.frames.saturating_sub(right.frames),
        left.encoded_bytes.saturating_sub(right.encoded_bytes),
    )
}

/// Startup credit lives in its own pool; only the rest occupies ordinary capacity.
fn ordinary_occupancy(entry: &super::delivery::Delivery) -> Counts {
    saturating_sub(entry.usage().total(), entry.startup_used())
}

const fn scale(counts: Counts, factor: u64) -> Counts {
    Counts::new(
        counts.frames.saturating_mul(factor),
        counts.encoded_bytes.saturating_mul(factor),
    )
}

const fn max(left: Counts, right: Counts) -> Counts {
    Counts::new(
        if left.frames > right.frames {
            left.frames
        } else {
            right.frames
        },
        if left.encoded_bytes > right.encoded_bytes {
            left.encoded_bytes
        } else {
            right.encoded_bytes
        },
    )
}

pub(super) struct CapacityPools {
    pub(super) startup: Counts,
    pub(super) regular: Counts,
}

impl ReceiveCapacity {
    pub const ENCODED_BYTES: usize = 16;

    pub const fn new(frames: u64, encoded_bytes: u64) -> Option<Self> {
        if frames == 0 || encoded_bytes == 0 {
            None
        } else {
            Some(Self(Counts::new(frames, encoded_bytes)))
        }
    }

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

    pub const fn receive_capacity(&self) -> Option<ReceiveCapacity> {
        self.receive_capacity
    }

    /// Additional ordinary credit needed to maintain a host-selected outstanding
    /// window. This is an advisory view, never a reservation or forwarding permit.
    pub fn window_credit(&self, flow: super::FlowKey, target: Counts) -> Option<Counts> {
        let (occupied, available) = self.window_view(flow)?;
        Some(saturating_sub(target, occupied).min(available))
    }

    /// Like `window_credit`, but splits a destination shared by many flows.
    /// Ordinary capacity stays reserved for `share.floor` per flow not yet
    /// retained, up to `share.flows`, and for every other open flow's shortfall
    /// below its floor. Later admissions, their first refill and siblings that
    /// are offered credit later in rotation therefore cannot be starved by idle
    /// windows: while the declaration holds every floor, each open flow can
    /// refill to its floor regardless of order. Floors count ordinary occupancy
    /// only, because consuming startup credit frees the startup pool rather than
    /// ordinary capacity. A declaration too small for the future reservation is
    /// split evenly instead.
    pub fn shared_window_credit(
        &self,
        flow: super::FlowKey,
        target: Counts,
        share: WindowShare,
    ) -> Option<Counts> {
        let (occupied, available) = self.window_view(flow)?;
        let retained = self.entries.usage().retained_flows as u64;
        let future = share.flows.saturating_sub(retained);
        let joining = scale(share.floor, future).min(saturating_sub(
            self.capacity_pools().regular,
            scale(share.floor, retained),
        ));
        let siblings = self
            .entries
            .iter()
            .filter(|entry| entry.key != flow && entry.phase() == super::Phase::Open)
            .fold(Counts::ZERO, |sum, entry| {
                let shortfall = saturating_sub(share.floor, ordinary_occupancy(entry));
                Counts::new(
                    sum.frames.saturating_add(shortfall.frames),
                    sum.encoded_bytes.saturating_add(shortfall.encoded_bytes),
                )
            });
        let reserve = Counts::new(
            joining.frames.saturating_add(siblings.frames),
            joining.encoded_bytes.saturating_add(siblings.encoded_bytes),
        );
        let entry = self.delivery(flow).ok()?;
        let floor = saturating_sub(share.floor, ordinary_occupancy(&entry)).min(available);
        let shared = saturating_sub(available, reserve);
        Some(saturating_sub(target, occupied).min(max(floor, shared)))
    }

    fn window_view(&self, flow: super::FlowKey) -> Option<(Counts, Counts)> {
        self.ensure_open().ok()?;
        let entry = self.delivery(flow).ok()?;
        if entry.phase() != super::Phase::Open {
            return None;
        }
        let available = self
            .receive_available()
            .ok()?
            .min(self.capacity_pools().regular.sub(self.regular_used()));
        Some((entry.usage().total(), available))
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
