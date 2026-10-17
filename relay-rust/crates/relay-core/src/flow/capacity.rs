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

impl<const N: usize, O: Observer> ConnectionLedger<N, O> {
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
                None => ledger.receive_capacity = Some(capacity),
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

    pub(super) fn receive_available(&self) -> Result<Counts, Refusal> {
        let capacity = self.receive_capacity.ok_or(Refusal::CapacityUndeclared)?;
        Ok(capacity
            .counts()
            .min(self.limits.total)
            .sub(self.usage.total()))
    }
}
