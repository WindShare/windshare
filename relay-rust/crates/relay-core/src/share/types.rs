use crate::{
    challenge::ChallengeSeed,
    clock::UnixMillis,
    error_frame::ErrorFrame,
    flow::{Counts, FlowKey, Grant, ReceivedFrame, SettlementNotice},
    identifiers::{RelaySessionId, ShareId},
    lifecycle::{ConnectionRef, Incarnation, OperationId, Record},
    registration::Registered,
    sessions::{OpaqueHeader, SessionKey},
    stop::Stopped,
};
use alloc::boxed::Box;
use alloc::vec::Vec;

/// Platform capacity is supplied explicitly; the shared engine has no platform defaults.
#[derive(Clone, Copy, Debug)]
pub struct Limits {
    pub connections: usize,
    /// Retained session bound, also bounding each destination's lazily backed flow slots.
    pub sessions: usize,
    pub delivery: crate::flow::Limits,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    Capacity,
    UnknownConnection,
    UnknownSession,
    Unavailable,
    SequenceExhausted,
    Configuration,
    Restore(crate::lifecycle::RestoreError),
    InvalidCheckpoint,
    CheckpointBusy,
    Flow(crate::flow::Refusal),
    Frame(crate::wire::FrameError),
    Forward(crate::sessions::ForwardRefusal),
}

#[derive(Debug)]
pub enum Input<'a> {
    Connected(ConnectionRef),
    Control {
        connection: ConnectionRef,
        frame: &'a [u8],
    },
    Challenge {
        request: ChallengeRequest,
        seed: ChallengeSeed,
    },
    Frame {
        source: ConnectionRef,
        header: OpaqueHeader,
    },
    Admitted {
        sender: ConnectionRef,
        frame: crate::sessions::SessionAdmitted,
    },
    Cancel {
        participant: ConnectionRef,
        session: RelaySessionId,
    },
    Grant {
        flow: FlowKey,
        credit: Counts,
    },
    Consumed {
        connection: ConnectionRef,
        session: RelaySessionId,
        total: Counts,
    },
    Settled {
        connection: ConnectionRef,
        session: RelaySessionId,
        total: Counts,
    },
    Discarded(FlowKey),
    Disconnected(ConnectionRef),
    /// Unlike a disconnect notification, this confirms no transport buffer retains bytes.
    Closed(ConnectionRef),
    Completed {
        id: WorkId,
        result: WorkResult,
    },
    Retry(WorkId),
    Tick,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ChallengeRequest {
    pub connection: ConnectionRef,
    pub(super) incarnation: Incarnation,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct WorkId {
    pub(super) incarnation: Incarnation,
    pub(super) sequence: u64,
}

impl WorkId {
    #[must_use]
    pub const fn incarnation(self) -> Incarnation {
        self.incarnation
    }

    #[must_use]
    pub const fn sequence(self) -> u64 {
        self.sequence
    }
}

/// A single serialized storage obligation. The host reports evidence, not a decision.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Work {
    pub id: WorkId,
    pub operation: Storage,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Storage {
    /// Atomically create this unresolved record and its authenticated descriptor.
    Publish {
        record: Box<Record>,
        descriptor: Vec<u8>,
    },
    /// Replace only the exact expected record. An already-applied next record is
    /// complete; a different record must never be overwritten unconditionally.
    CompareExchange {
        expected: Option<Box<Record>>,
        next: Option<Box<Record>>,
    },
    /// Quiesce this attempt's writes and remove its descriptor/resources. Delete a
    /// lifecycle record only if it is this exact attempt's unresolved publication.
    RemovePublication(OperationId),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum WorkResult {
    Complete,
    /// Publication only: proves that no record, resource or outstanding write remains.
    Aborted,
    Uncertain,
}

#[derive(Debug)]
pub enum Output {
    ChallengeNeeded(ChallengeRequest),
    Send {
        connection: ConnectionRef,
        control: Control,
    },
    /// Metadata receipt only. Payload ownership never moves into the engine.
    Received(ReceivedFrame),
    Discard(FlowKey),
    Close(ConnectionRef),
    Work(Work),
    /// Retry this exact work with platform backoff; it is not automatically successful.
    Retry(WorkId),
    Reload,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Control {
    Challenge(Vec<u8>),
    Error(ErrorFrame),
    Registered(Registered),
    Descriptor {
        session: SessionKey,
        record: Box<Record>,
    },
    Grant(Grant),
    /// Shares the destination's ordered writer with submitted opaque frames.
    Settlement(SettlementNotice),
    Stopped(Stopped),
}

/// Outputs are ordered within a turn. Independent connections may execute in parallel.
#[derive(Debug, Default)]
#[must_use]
pub struct Turn {
    pub outputs: Vec<Output>,
    pub deadline: Option<UnixMillis>,
    pub refusal: Option<Error>,
}

/// The synchronous handoff occurs while the whole relay remains exclusively borrowed.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Submission {
    Complete,
    Uncertain,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Identity {
    pub relay: crate::identifiers::RelayIdentity,
    pub share: ShareId,
    pub incarnation: Incarnation,
}
