//! Bounded native output ownership and synchronous kernel submission.
//!
//! Queueing a receipt does not authorize sending it. Only `submit_next` transfers
//! bytes to the writer, while the caller exclusively owns the share kernel.

use crate::config::{
    CAPACITY_NOTICE_QUEUE_FRAMES, CONNECTION_DATA_FRAMES, CONTROL_QUEUE_FRAMES, DATA_QUEUE_BYTES,
    MAX_CONTROL_BYTES, SUBMITTED_DATA_FRAMES,
};
use crate::node::{Runtime, access::Bearer};
use anyhow::{Result, ensure};
use std::sync::Arc;
use windshare_relay_access::Attribution;
mod pending;
use tokio::sync::{Semaphore, mpsc, watch};
use windshare_relay_core::{
    clock::UnixMillis,
    flow::{Counts, FlowKey, ReceivedFrame},
    lifecycle::ConnectionRef,
    observation::Observer,
    sessions::CapacityWait,
    share::{RelayStep, ShareRelay, Submission},
};

mod admission;
mod limits;
pub use admission::{Admission, AdmissionPriority, Capacity};
pub use limits::ConnectionLimits;
mod runtime;
mod transport;
mod write_window;
pub use write_window::{WritePermit, WriteWindow};
mod writer;
pub use runtime::{run, stopped};
pub use writer::Writer;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Closure {
    Open,
    Drain,
    Abort,
}

/// A share actor's exclusive owner of one destination's unsubmitted payloads.
/// Dropping it aborts the connection; orderly terminal replies use `finish`.
pub struct Outbox {
    bearer: Option<Bearer>,
    source: Option<std::net::IpAddr>,
    connection: ConnectionRef,
    pending: pending::Queues,
    node: Runtime,
    pending_bytes: usize,
    pending_limit: Counts,
    ordered: mpsc::Sender<writer::Frame>,
    probe: watch::Sender<Option<Vec<u8>>>,
    closure: watch::Sender<Closure>,
    control_slots: Arc<Semaphore>,
    capacity_notice_slots: Arc<Semaphore>,
}

pub fn channel(
    connection: ConnectionRef,
    node: Runtime,
) -> (Outbox, Writer, watch::Receiver<Closure>) {
    let ordered_slots = CONTROL_QUEUE_FRAMES + CAPACITY_NOTICE_QUEUE_FRAMES + SUBMITTED_DATA_FRAMES;
    let (ordered, incoming) = mpsc::channel(ordered_slots);
    let (probe, probes) = watch::channel(None);
    let (closure, closing) = watch::channel(Closure::Open);
    let outbox = Outbox {
        bearer: None,
        source: None,
        connection,
        pending: pending::Queues::default(),
        node,
        pending_bytes: 0,
        pending_limit: Counts::new(CONNECTION_DATA_FRAMES as u64, DATA_QUEUE_BYTES as u64),
        ordered,
        probe,
        closure,
        control_slots: Arc::new(Semaphore::new(CONTROL_QUEUE_FRAMES)),
        capacity_notice_slots: Arc::new(Semaphore::new(CAPACITY_NOTICE_QUEUE_FRAMES)),
    };
    (outbox, Writer::new(incoming, probes), closing)
}

impl Outbox {
    pub fn set_wake(&mut self, wake: Arc<tokio::sync::Notify>) {
        self.pending.set_wake(wake);
    }
    pub fn with_admission(mut self, admission: &Admission) -> Self {
        self.bearer = admission.bearer().cloned();
        self.source = Some(admission.source());
        self
    }
    pub(crate) const fn bearer(&self) -> Option<&Bearer> {
        self.bearer.as_ref()
    }
    pub const fn source(&self) -> Option<std::net::IpAddr> {
        self.source
    }

    /// Retains a kernel receipt and its exact payload until submission or discard.
    ///
    /// # Errors
    /// Refuses a different destination, mismatched size or exhausted frame/byte budget.
    pub fn queue(
        &mut self,
        receipt: ReceivedFrame,
        bytes: Vec<u8>,
        attribution: Attribution,
    ) -> Result<()> {
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
        let length = bytes.len();
        // Retain exactly the charged bytes, not excess capacity from an ingress buffer.
        self.pending.push(
            pending::Pending {
                receipt,
                bytes: bytes.into_boxed_slice(),
                queued_at: tokio::time::Instant::now(),
            },
            &self.node.governor,
            attribution,
        )?;
        self.pending_bytes += length;
        Ok(())
    }

    /// Reserves bounded writer capacity, then rechecks authority and commits in
    /// the same synchronous call. Socket and storage waits cannot enter this boundary.
    pub fn submit_next<O: Observer>(
        &mut self,
        relay: &mut ShareRelay<O>,
        now: UnixMillis,
        attribution: Attribution,
    ) -> Option<RelayStep> {
        if self.pending.is_empty() {
            return None;
        }
        let reserved = self.ordered.try_reserve().ok()?;
        let (pending, permit) = self.pending.take(&self.node.governor)?;
        self.pending_bytes -= pending.bytes.len();
        // This bounded mailbox is the host handoff, not a cached permit. Anything
        // inside it is already charged as submitted and may outlive revocation.
        let result = self.node.access.with_forwarding(
            pending.receipt.flow().session().share(),
            attribution,
            || {
                relay.submit(&pending.receipt, now, |_| {
                    reserved.send(
                        writer::Frame::new(
                            pending.bytes.into_vec(),
                            permit.slot,
                            Some(permit.bytes),
                            Some(pending.receipt.flow().session()),
                        )
                        .attributed(attribution),
                    );
                    Submission::Complete
                })
            },
        );
        Some(result.unwrap_or_else(|| {
            relay.handle(
                windshare_relay_core::share::Input::Disconnected(self.connection),
                now,
            )
        }))
    }

    /// Drops only this exact flow's unsubmitted payloads before its discard receipt.
    pub fn discard(&mut self, flow: FlowKey) {
        self.pending_bytes -= self.pending.discard(flow);
    }

    pub fn capacity_updates(&mut self, now: tokio::time::Instant) -> Vec<(FlowKey, u32)> {
        self.pending.capacity_updates(now)
    }

    pub fn control_available(&self) -> bool {
        self.control_slots.available_permits() != 0 && !self.ordered.is_closed()
    }

    pub fn control(&self, bytes: Vec<u8>) -> Result<()> {
        ensure!(
            bytes.len() <= MAX_CONTROL_BYTES,
            "oversized outgoing control"
        );
        let slot = Arc::clone(&self.control_slots).try_acquire_owned()?;
        self.ordered
            .try_send(writer::Frame::new(bytes, slot, None, None))?;
        Ok(())
    }

    /// Congestion is short-lived advice. Its own slots preserve the capacity for
    /// required protocol controls while keeping notices in wire order.
    pub fn capacity_wait(&self, notice: CapacityWait) -> Result<()> {
        let slot = Arc::clone(&self.capacity_notice_slots).try_acquire_owned()?;
        self.ordered.try_send(writer::Frame::new(
            notice.encode()?.to_vec(),
            slot,
            None,
            None,
        ))?;
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
