//! Bounded native output ownership and synchronous kernel submission.
//!
//! Queueing a receipt does not authorize sending it. Only `submit_next` transfers
//! bytes to the writer, while the caller exclusively owns the share kernel.

use crate::config::{
    CONTROL_QUEUE_FRAMES, DATA_QUEUE_BYTES, DATA_QUEUE_FRAMES, MAX_CONTROL_BYTES,
    SUBMITTED_DATA_BYTES, SUBMITTED_DATA_FRAMES,
};
use anyhow::{Result, ensure};
use std::{collections::VecDeque, sync::Arc};
use tokio::sync::{Semaphore, mpsc, watch};
use windshare_relay_core::{
    clock::UnixMillis,
    flow::{Counts, FlowKey, ReceivedFrame},
    lifecycle::ConnectionRef,
    observation::Observer,
    share::{ShareRelay, Submission, Turn},
};

mod runtime;
mod writer;
pub use runtime::{run, stopped};
pub use writer::Writer;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Closure {
    Open,
    Drain,
    Abort,
}

struct Pending {
    receipt: ReceivedFrame,
    bytes: Box<[u8]>,
}

/// A share actor's exclusive owner of one destination's unsubmitted payloads.
/// Dropping it aborts the connection; orderly terminal replies use `finish`.
pub struct Outbox {
    connection: ConnectionRef,
    pending: VecDeque<Pending>,
    pending_bytes: usize,
    pending_limit: Counts,
    ordered: mpsc::Sender<writer::Frame>,
    probe: watch::Sender<Option<Vec<u8>>>,
    closure: watch::Sender<Closure>,
    control_slots: Arc<Semaphore>,
    data_slots: Arc<Semaphore>,
    data_bytes: Arc<Semaphore>,
}

pub fn channel(connection: ConnectionRef) -> (Outbox, Writer, watch::Receiver<Closure>) {
    let (ordered, incoming) = mpsc::channel(CONTROL_QUEUE_FRAMES + SUBMITTED_DATA_FRAMES);
    let (probe, probes) = watch::channel(None);
    let (closure, closing) = watch::channel(Closure::Open);
    let outbox = Outbox {
        connection,
        pending: VecDeque::new(),
        pending_bytes: 0,
        pending_limit: Counts::new(DATA_QUEUE_FRAMES as u64, DATA_QUEUE_BYTES as u64),
        ordered,
        probe,
        closure,
        control_slots: Arc::new(Semaphore::new(CONTROL_QUEUE_FRAMES)),
        data_slots: Arc::new(Semaphore::new(SUBMITTED_DATA_FRAMES)),
        data_bytes: Arc::new(Semaphore::new(SUBMITTED_DATA_BYTES)),
    };
    (outbox, Writer::new(incoming, probes), closing)
}

impl Outbox {
    /// Retains a kernel receipt and its exact payload until submission or discard.
    ///
    /// # Errors
    /// Refuses a different destination, mismatched size or exhausted frame/byte budget.
    #[cfg_attr(
        not(test),
        expect(
            dead_code,
            reason = "JOIN ingress awaits delivery recovery and client runtime integration"
        )
    )]
    pub fn queue(&mut self, receipt: ReceivedFrame, bytes: Vec<u8>) -> Result<()> {
        ensure!(
            receipt.flow().destination() == self.connection,
            "wrong output destination"
        );
        ensure!(
            receipt.encoded_bytes() == bytes.len() as u64,
            "receipt size mismatch"
        );
        ensure!(
            (self.pending.len() as u64) < self.pending_limit.frames
                && bytes.len() as u64
                    <= self
                        .pending_limit
                        .encoded_bytes
                        .saturating_sub(self.pending_bytes as u64),
            "unsubmitted output capacity exhausted"
        );
        self.pending_bytes += bytes.len();
        // Retain exactly the charged bytes, not excess capacity from an ingress buffer.
        self.pending.push_back(Pending {
            receipt,
            bytes: bytes.into_boxed_slice(),
        });
        Ok(())
    }

    /// Reserves bounded writer capacity, then rechecks authority and commits in
    /// the same synchronous call. Socket and storage waits cannot enter this boundary.
    pub fn submit_next<O: Observer>(
        &mut self,
        relay: &mut ShareRelay<O>,
        now: UnixMillis,
    ) -> Option<Turn> {
        let next = self.pending.front()?;
        let slot = Arc::clone(&self.data_slots).try_acquire_owned().ok()?;
        let size = u32::try_from(next.bytes.len()).ok()?;
        let bytes = Arc::clone(&self.data_bytes)
            .try_acquire_many_owned(size)
            .ok()?;
        let reserved = self.ordered.try_reserve().ok()?;
        let pending = self.pending.pop_front().expect("inspected front");
        self.pending_bytes -= pending.bytes.len();
        // This bounded mailbox is the host handoff, not a cached permit. Anything
        // inside it is already charged as submitted and may outlive revocation.
        Some(relay.submit(&pending.receipt, now, |_| {
            reserved.send(writer::Frame::new(
                pending.bytes.into_vec(),
                slot,
                Some(bytes),
            ));
            Submission::Complete
        }))
    }

    /// Drops only this exact flow's unsubmitted payloads before its discard receipt.
    pub fn discard(&mut self, flow: FlowKey) {
        self.pending.retain(|pending| {
            if pending.receipt.flow() == flow {
                self.pending_bytes -= pending.bytes.len();
                false
            } else {
                true
            }
        });
    }

    pub fn control(&self, bytes: Vec<u8>) -> Result<()> {
        ensure!(
            bytes.len() <= MAX_CONTROL_BYTES,
            "oversized outgoing control"
        );
        let slot = Arc::clone(&self.control_slots).try_acquire_owned()?;
        self.ordered
            .try_send(writer::Frame::new(bytes, slot, None))?;
        Ok(())
    }

    pub fn probe(&self, bytes: Vec<u8>) -> Result<()> {
        ensure!(
            bytes.len() <= MAX_CONTROL_BYTES,
            "oversized probe acknowledgement"
        );
        // Probe replies carry increasing nonces; replacing an unsent one is safe
        // and keeps heartbeats independent of the data queue's capacity and order.
        self.probe.send(Some(bytes))?;
        Ok(())
    }

    pub fn finish(self) {
        self.closure.send_replace(Closure::Drain);
    }
}

impl Drop for Outbox {
    fn drop(&mut self) {
        self.pending.clear();
        self.pending_bytes = 0;
        self.closure.send_if_modified(|closure| {
            if *closure == Closure::Open {
                *closure = Closure::Abort;
                true
            } else {
                false
            }
        });
    }
}

#[cfg(test)]
#[path = "queue_tests.rs"]
mod tests;
