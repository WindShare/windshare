//! Bounded delivery accounting for one destination connection generation.
//!
//! [`crate::share::ShareRelay`] composes this ledger with live session authority,
//! FIFO submission, retirement and deadline scheduling. Hosts choose policy and
//! retain payloads; every grant also fits the destination's handshake capacity.
//!
//! Retirement revokes unused credit immediately. Unsubmitted payloads remain
//! charged until discard confirmation, and submitted bytes remain charged until
//! consumption, settlement or confirmed physical closure. Cumulative feedback
//! binds to exact session, direction and destination generations.
//!
//! [`LedgerCheckpoint`] preserves cumulative accounting and original deadlines
//! without payloads. Aggregate recovery validates open routes against reinstated
//! sessions and retains retired flows until cleanup. A surviving connection must
//! never be assigned an empty replacement ledger. This module defines accounting
//! contracts; [`DeliveryConsumed`] freezes cumulative feedback bytes for the
//! coordinated client cutover. [`SettlementNotice`] and [`SettlementAck`] carry
//! frozen retirement totals separately from ordinary consumption.

mod accounting;
mod capacity;
pub use capacity::ReceiveCapacity;
mod checkpoint;
mod checkpoint_codec;
pub use checkpoint::{CheckpointError, LedgerCheckpoint};
mod consumed;
pub use consumed::DeliveryConsumed;
mod delivery;
mod forwarding;
mod observation;
mod retirement;
mod settlement;
pub use settlement::{SettlementAck, SettlementNotice};
mod slots;
#[cfg(test)]
#[path = "slots_tests.rs"]
mod slots_tests;
mod totals;
use slots::DeliverySlots;
pub use slots::MetadataUsage;

pub use accounting::{ConfigurationError, Counts, Limits, Usage};
pub use delivery::{FlowKey, Grant, Phase, ReceivedFrame, Settlement, Snapshot};
pub use observation::{Decision, Input, Observation, Observer};
pub use retirement::Feedback;

use crate::{
    clock::UnixMillis,
    identifiers::RelayIdentity,
    lifecycle::{ConnectionRef, Observer as LifecycleObserver},
    sessions::ValidatedForward,
};
use delivery::Delivery;

/// Exact destination lifetime that must terminate after stalled cleanup.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[must_use = "terminate this connection and confirm closure before reclaiming its occupancy"]
pub struct CloseRequest {
    /// A different connection generation must never be closed for this request.
    pub destination: ConnectionRef,
    /// The retirement whose original deadline expired.
    pub flow: FlowKey,
    /// Traffic, retries and partial consumption never renew this deadline.
    pub deadline: UnixMillis,
}

/// Why an accounting operation was refused.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Refusal {
    /// No REGISTER/RESUME/JOIN capacity declaration has bound this connection.
    CapacityUndeclared,
    /// A surviving connection cannot renegotiate or reset its receive budget.
    CapacityChanged,
    /// The authorization or feedback names a different relay/destination lifetime.
    WrongDestination,
    /// The flow handle was removed or its metadata slot now holds another route.
    StaleFlow,
    /// The supplied live route differs from the flow's exact route.
    WrongRoute,
    /// The opaque envelope names another wire session.
    WrongSession,
    /// This direction is already registered, including unfinished retirement.
    AlreadyOpen,
    /// Bounded metadata storage is full.
    FlowCapacity,
    /// The allocator could not back another slot within the configured bound.
    FlowAllocation,
    /// Neither protected startup nor spare ordinary capacity can fund admission.
    StartupCapacity,
    /// Ordinary grants would exceed their separate portion of the total bound.
    DeliveryCapacity,
    /// A grant must increase at least one dimension.
    EmptyGrant,
    /// A cumulative counter cannot wrap, even over a very long session.
    CounterExhausted,
    /// A source exceeded its reserved frame or byte allowance.
    InsufficientCredit,
    /// A receipt was already submitted or is ahead of an earlier queued receipt.
    SubmissionOrder,
    /// Retirement has fenced new grants, receives and submissions.
    Retired,
    /// Cleanup cannot complete before session retirement.
    NotRetired,
    /// Final settlement cannot precede host confirmation of discarded payloads.
    CleanupPending,
    /// Consumption is inconsistent, regresses in one dimension, or exceeds submission.
    InvalidConsumption,
    /// Final consumption must exactly match the frozen submitted totals.
    InvalidSettlement,
    /// Settlement timed out; execute `close_request()` and confirm physical closure.
    Closing,
    /// This lifetime cannot be reopened.
    Closed,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum ConnectionState {
    Open,
    Closing(CloseRequest),
    Closed,
}

/// One move-only ledger with a runtime metadata bound and lazily allocated slots.
///
/// Lookup on the forwarding path is constant time through opaque `FlowKey`
/// slots. Only admission, retirement lookup and deadline selection scan storage.
#[derive(Debug)]
pub struct ConnectionLedger<O> {
    relay: RelayIdentity,
    destination: ConnectionRef,
    limits: Limits,
    receive_capacity: Option<ReceiveCapacity>,
    entries: DeliverySlots,
    usage: Usage,
    startup_used: Counts,
    state: ConnectionState,
    next_expiration: Option<CloseRequest>,
    checkpoint_changed: bool,
    observer: O,
}

impl<O: Observer> ConnectionLedger<O> {
    pub(crate) const fn observer_mut(&mut self) -> &mut O {
        &mut self.observer
    }

    /// Starts accounting for a newly opened physical connection, never a live reset.
    /// No delivery grant is possible until its handshake declares receive capacity.
    /// The metadata bound reserves no slots; backing storage grows on flow admission.
    ///
    /// # Errors
    ///
    /// Rejects zero metadata slots. Capacities are validated by `Limits::new`.
    pub fn new(
        relay: RelayIdentity,
        destination: ConnectionRef,
        limits: Limits,
        max_flows: usize,
        observer: O,
    ) -> Result<Self, ConfigurationError> {
        let entries = DeliverySlots::new(max_flows)?;
        Ok(Self {
            relay,
            destination,
            limits,
            receive_capacity: None,
            entries,
            usage: Usage::default(),
            startup_used: Counts::ZERO,
            state: ConnectionState::Open,
            next_expiration: None,
            checkpoint_changed: false,
            observer,
        })
    }

    /// Connection-wide usage, including retired but unsettled flows.
    #[must_use]
    pub const fn usage(&self) -> Usage {
        self.usage
    }

    /// Retained flows and allocated slots, separate from payload capacity.
    #[must_use]
    pub const fn metadata_usage(&self) -> MetadataUsage {
        self.entries.usage()
    }

    /// Occupied part of the protected startup pool.
    #[must_use]
    pub const fn startup_used(&self) -> Counts {
        self.startup_used
    }

    /// Ordinary occupancy cannot borrow from the startup pool.
    #[must_use]
    pub fn regular_used(&self) -> Counts {
        self.usage.total().sub(self.startup_used)
    }

    /// Read-only counters for a still-retained exact direction.
    #[must_use]
    pub fn snapshot(&self, flow: FlowKey) -> Option<Snapshot> {
        self.delivery(flow).ok().map(Snapshot::from)
    }

    /// Allows hosts/tests to inspect their own tracing backend.
    #[must_use]
    pub const fn observer(&self) -> &O {
        &self.observer
    }

    fn delivery(&self, flow: FlowKey) -> Result<Delivery, Refusal> {
        self.entries
            .get(flow.slot)
            .filter(|entry| entry.key == flow)
            .ok_or(Refusal::StaleFlow)
    }

    fn check_destination(&self, flow: FlowKey, from: ConnectionRef) -> Result<(), Refusal> {
        if flow.session().relay() != self.relay
            || flow.destination() != self.destination
            || from != self.destination
        {
            return Err(Refusal::WrongDestination);
        }
        Ok(())
    }

    fn live_delivery<L: LifecycleObserver>(
        &self,
        flow: FlowKey,
        route: &ValidatedForward<'_, L>,
    ) -> Result<Delivery, Refusal> {
        self.ensure_open()?;
        self.check_destination(flow, route.permit().destination())?;
        if flow.permit != route.permit() {
            return Err(Refusal::WrongRoute);
        }
        let entry = self.delivery(flow)?;
        if entry.state != delivery::State::Open {
            return Err(Refusal::Retired);
        }
        Ok(entry)
    }

    const fn ensure_open(&self) -> Result<(), Refusal> {
        match self.state {
            ConnectionState::Open => Ok(()),
            ConnectionState::Closing(_) => Err(Refusal::Closing),
            ConnectionState::Closed => Err(Refusal::Closed),
        }
    }

    fn replace(&mut self, slot: usize, next: Option<Delivery>) {
        let before = self.entries.get(slot);
        self.checkpoint_changed = self.checkpoint_changed || before != next;
        self.usage = self.usage.replace(
            before.map_or_else(Usage::default, Delivery::usage),
            next.map_or_else(Usage::default, Delivery::usage),
        );
        self.startup_used = self
            .startup_used
            .sub(before.map_or(Counts::ZERO, Delivery::startup_used))
            .add(next.map_or(Counts::ZERO, Delivery::startup_used));
        self.entries.replace(slot, next);
        if before.and_then(Delivery::deadline) != next.and_then(Delivery::deadline) {
            // Only retirement/settlement changes deadlines. Forwarding and
            // consumption keep constant-time event-entry expiration checks.
            self.next_expiration = self
                .entries
                .iter()
                .filter_map(|entry| {
                    entry.deadline().map(|deadline| CloseRequest {
                        destination: self.destination,
                        flow: entry.key,
                        deadline,
                    })
                })
                .min_by_key(|request| request.deadline);
        }
        debug_assert!(self.capacity_pools().startup.covers(self.startup_used));
        debug_assert!(self.capacity_pools().regular.covers(self.regular_used()));
        debug_assert!(
            self.receive_capacity
                .is_some_and(|capacity| capacity.counts().covers(self.usage.total()))
                || self.usage.total() == Counts::ZERO
        );
    }

    fn record(&mut self, input: Input, decision: Decision, now: UnixMillis) {
        self.observer.observe(
            Observation {
                at: now,
                relay: self.relay,
                destination: self.destination,
                input,
                decision,
                usage: self.usage,
                startup_used: self.startup_used,
                metadata: self.metadata_usage(),
            }
            .into(),
        );
    }

    fn run<T>(
        &mut self,
        input: Input,
        now: UnixMillis,
        apply: impl FnOnce(&mut Self) -> Result<T, Refusal>,
    ) -> Result<T, Refusal> {
        self.expire(now);
        let result = apply(self);
        if let Err(reason) = &result {
            self.record(input, Decision::Refused(*reason), now);
        }
        result
    }
}
