//! Shared diagnostic vocabulary, with no logger or ciphertext in the kernel.

use crate::{
    clock::UnixMillis,
    identifiers::RelayIdentity,
    lifecycle::ConnectionRef,
    sessions::{ForwardPermit, OpaqueHeader, Retirement},
};

use super::{CloseRequest, Counts, FlowKey, Refusal, Usage};

/// Input correlation retained on refusals and resource lifecycle transitions.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Input {
    /// Attempt to reserve the initial delivery window before receiving any frame.
    Open(ForwardPermit),
    /// Ordinary source allowance, backed by the non-startup capacity.
    Grant { flow: FlowKey, credit: Counts },
    /// Only decoded routing metadata; the host retains the actual bytes.
    Receive { flow: FlowKey, header: OpaqueHeader },
    /// FIFO handoff to the destination host.
    Submit { flow: FlowKey, ordinal: u64 },
    /// Cumulative application consumption, never socket-write completion.
    Consume {
        flow: FlowKey,
        from: ConnectionRef,
        total: Counts,
    },
    /// Final consume/discard acknowledgement of the ordered retirement notice.
    Settle {
        flow: FlowKey,
        from: ConnectionRef,
        total: Counts,
    },
    /// Session authority has already been revoked.
    Retire(Retirement),
    /// Host confirms all unsent payloads for this flow are gone.
    Discarded(FlowKey),
    /// An absolute timer/alarm or event-entry time check.
    Tick,
    /// The named physical connection has finished terminating.
    Closed(ConnectionRef),
}

/// Milestones only; ordinary forwarding and consumption do not trace per frame.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Decision {
    /// Startup capacity was reserved before issuing the first allowance.
    Opened(FlowKey),
    /// An operation was refused without changing its accounting.
    Refused(Refusal),
    /// Unused grants were revoked; accepted payloads still belong to the host.
    Retiring(FlowKey),
    /// Unsent payloads are gone; submitted occupancy still needs settlement.
    AwaitingSettlement(FlowKey),
    /// All occupancy for a retired flow has been reclaimed.
    Settled(FlowKey),
    /// Cleanup stalled; capacity remains charged until closure is confirmed.
    Closing(CloseRequest),
    /// Exact-generation physical closure released the remaining occupancy.
    Closed { reclaimed: Counts },
}

/// One bounded decision with enough identity and occupancy to reconstruct it.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Observation {
    /// Injected wall-clock time.
    pub at: UnixMillis,
    /// Relay namespace of this connection.
    pub relay: RelayIdentity,
    /// Exact destination connection lifetime.
    pub destination: ConnectionRef,
    /// Includes original identity/counts even for stale or malformed feedback.
    pub input: Input,
    /// Outcome or transition.
    pub decision: Decision,
    /// Occupancy after the decision.
    pub usage: Usage,
    /// Portion still charged to the protected startup pool.
    pub startup_used: Counts,
}

/// A host selects a synchronous structured tracing backend.
pub trait Observer {
    /// Must not re-enter the ledger.
    fn observe(&mut self, observation: Observation);
}

impl Observer for () {
    fn observe(&mut self, _: Observation) {}
}
