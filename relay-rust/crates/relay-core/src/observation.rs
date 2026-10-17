//! One diagnostic boundary for both hosts, retaining each domain's precise events.

use crate::{clock::UnixMillis, flow, lifecycle, sessions};
use alloc::vec::Vec;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Observation {
    Lifecycle(lifecycle::Observation),
    Session(sessions::Observation),
    Delivery(flow::Observation),
    Relay {
        at: UnixMillis,
        relay: crate::identifiers::RelayIdentity,
        share: crate::identifiers::ShareId,
        incarnation: lifecycle::Incarnation,
        connection: Option<lifecycle::ConnectionRef>,
        decision: RelayDecision,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RelayDecision {
    Connected,
    Closing,
    Disconnected,
    Closed,
    ProbeAccepted {
        nonce: u64,
    },
    ProbeRejected {
        nonce: u64,
        last_nonce: u64,
    },
    ConnectionSilent {
        last_probe_nonce: u64,
        last_inbound: UnixMillis,
        deadline: UnixMillis,
    },
    ResponsibilityReleased,
    RecoveryIsolated {
        reserved: flow::Counts,
    },
    Retired {
        residual: flow::Counts,
    },
    RetirementBlocked {
        usage: crate::share::ConnectionUsage,
        residual: flow::Counts,
        generation_limit: usize,
        residual_limit: flow::Counts,
    },
    ResponsibilityRestored {
        communicating: bool,
        usage: flow::Usage,
    },
    StopAcknowledged {
        stop_id: crate::identifiers::StopId,
        unsettled_generations: usize,
    },
    Capacity,
    StaleCompletion(crate::share::WorkId),
    StorageUncertain(crate::share::WorkId),
    WorkIssued(crate::share::WorkId),
    WorkCompleted {
        id: crate::share::WorkId,
        result: crate::share::WorkResult,
    },
    HandshakeClosed(crate::handshake::CloseReason),
    Restored {
        connections: usize,
        retained_generations: usize,
    },
    InvalidSnapshot,
    CheckpointingEnabled {
        payload_bytes: usize,
    },
    CheckpointsFlushed {
        connections: usize,
        encoded_bytes: usize,
    },
    CheckpointWriteFailed,
    Rejected(crate::share::Error),
}

/// Synchronous sinks must not re-enter the relay. Payload bytes never enter this API.
pub trait Observer {
    fn observe(&mut self, observation: Observation);
}

impl Observer for () {
    fn observe(&mut self, _: Observation) {}
}

impl From<lifecycle::Observation> for Observation {
    fn from(value: lifecycle::Observation) -> Self {
        Self::Lifecycle(value)
    }
}
impl From<sessions::Observation> for Observation {
    fn from(value: sessions::Observation) -> Self {
        Self::Session(value)
    }
}
impl From<flow::Observation> for Observation {
    fn from(value: flow::Observation) -> Self {
        Self::Delivery(value)
    }
}

// A single transition can emit several domain decisions. Drain before executing
// its consequences, preserving order without a shared logger, lock or runtime.
#[derive(Debug, Default)]
pub(crate) struct Journal(pub Vec<Observation>);
impl Observer for Journal {
    fn observe(&mut self, observation: Observation) {
        self.0.push(observation);
    }
}
