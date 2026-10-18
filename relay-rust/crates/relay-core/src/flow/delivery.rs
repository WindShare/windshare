//! Per-direction accounting uses ordered prefixes, without retaining payloads.

use crate::{
    clock::UnixMillis,
    lifecycle::ConnectionRef,
    sessions::{Direction, ForwardPermit, SessionKey},
};

use super::{Counts, Usage};

/// Stable handle for delayed work. Slot reuse cannot retarget its exact route.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct FlowKey {
    pub(super) slot: usize,
    pub(super) permit: ForwardPermit,
}

impl FlowKey {
    /// Exact relay/share/admission and both connection generations.
    #[must_use]
    pub const fn session(self) -> SessionKey {
        self.permit.session()
    }

    /// This ledger accounts for only this delivery direction.
    #[must_use]
    pub const fn direction(self) -> Direction {
        self.permit.direction()
    }

    /// Connection whose consumption can release this flow's submitted occupancy.
    #[must_use]
    pub const fn destination(self) -> ConnectionRef {
        self.permit.destination()
    }

    /// Revalidate this route through the live session before granting or forwarding.
    #[must_use]
    pub const fn permit(self) -> ForwardPermit {
        self.permit
    }
}

/// A reservation already charged to the connection, to issue to the source once.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[must_use = "issue the reserved grant once, or retire the flow if delivery fails"]
pub struct Grant {
    /// Exact direction and session receiving the allowance.
    pub flow: FlowKey,
    /// Independent frame and byte increments.
    pub credit: Counts,
}

impl Grant {
    pub const ENCODED_BYTES: usize = 8 + 8 + 4 + 4;

    /// Encode a reserved allowance, independently of host window policy.
    ///
    /// # Errors
    /// Rejects empty grants or increments wider than the wire's u32 fields.
    pub fn encode(self) -> Result<[u8; Self::ENCODED_BYTES], crate::FrameError> {
        use crate::{FrameError, wire::Writer};
        if self.credit == Counts::ZERO {
            return Err(FrameError::Field);
        }
        let frames = u32::try_from(self.credit.frames).map_err(|_| FrameError::Field)?;
        let bytes = u32::try_from(self.credit.encoded_bytes).map_err(|_| FrameError::Field)?;
        let mut out = Writer::with_header(*b"WS2W", 0);
        out.put(self.flow.session().wire_id().as_bytes())
            .put(&frames.to_be_bytes())
            .put(&bytes.to_be_bytes());
        Ok(out.into_frame())
    }
}

/// Receipt for one accepted frame; it cannot be cloned into multiple submissions.
#[derive(Debug, Eq, PartialEq)]
#[must_use = "retain with the payload until submission or confirmed retirement cleanup"]
pub struct ReceivedFrame {
    pub(super) flow: FlowKey,
    pub(super) ordinal: u64,
    pub(super) encoded_bytes: u64,
}

impl ReceivedFrame {
    /// Keep this exact key through any asynchronous queueing.
    #[must_use]
    pub const fn flow(&self) -> FlowKey {
        self.flow
    }

    /// Wire bytes that the host must retain and submit with this receipt.
    #[must_use]
    pub const fn encoded_bytes(&self) -> u64 {
        self.encoded_bytes
    }
}

/// Ordered retirement notice, emitted only after unsent payloads have been dropped.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[must_use = "order the retirement notice after all submitted frames on this destination"]
pub struct Settlement {
    /// Includes the destination lifetime, admission and direction.
    pub flow: FlowKey,
    /// Final cumulative count the destination must consume or discard.
    pub submitted: Counts,
}

/// Bounded resource lifecycle, independent of the session's routing authority.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Phase {
    /// May grant, receive and submit with live routing authority.
    Open,
    /// Authority is gone, but the host has not confirmed dropping unsent payloads.
    Discarding,
    /// Only consumption, final settlement or confirmed closure can release occupancy.
    Settling,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum State {
    Open,
    Discarding { deadline: UnixMillis },
    Settling { deadline: UnixMillis },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) struct Delivery {
    pub key: FlowKey,
    pub startup: Counts,
    pub granted: Counts,
    pub received: Counts,
    pub submitted: Counts,
    pub consumed: Counts,
    pub state: State,
}

impl Delivery {
    pub const fn new(key: FlowKey, credit: Counts, startup: Counts) -> Self {
        Self {
            key,
            startup,
            granted: credit,
            received: Counts::ZERO,
            submitted: Counts::ZERO,
            consumed: Counts::ZERO,
            state: State::Open,
        }
    }

    pub const fn phase(self) -> Phase {
        match self.state {
            State::Open => Phase::Open,
            State::Discarding { .. } => Phase::Discarding,
            State::Settling { .. } => Phase::Settling,
        }
    }

    pub const fn deadline(self) -> Option<UnixMillis> {
        match self.state {
            State::Open => None,
            State::Discarding { deadline } | State::Settling { deadline } => Some(deadline),
        }
    }

    pub fn usage(self) -> Usage {
        Usage {
            granted: if self.state == State::Open {
                self.granted.sub(self.received)
            } else {
                Counts::ZERO
            },
            received: if matches!(self.state, State::Settling { .. }) {
                Counts::ZERO
            } else {
                self.received.sub(self.submitted)
            },
            submitted: self.submitted.sub(self.consumed),
        }
    }

    pub const fn startup_used(self) -> Counts {
        // Startup is the first prefix in each independent dimension. Enforced
        // FIFO submission lets cumulative consumption release it without per-frame
        // allocation metadata or turning the startup pool into replenishment.
        let retained = match self.state {
            State::Open => self.granted,
            State::Discarding { .. } => self.received,
            State::Settling { .. } => self.submitted,
        };
        retained
            .min(self.startup)
            .sub(self.consumed.min(self.startup))
    }

    pub const fn settlement(self) -> Settlement {
        Settlement {
            flow: self.key,
            submitted: self.submitted,
        }
    }
}

/// A flow snapshot contains counters only; it cannot be restored as live authority.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Snapshot {
    /// Current cleanup phase.
    pub phase: Phase,
    /// All resource stages for this direction.
    pub usage: Usage,
    /// Cumulative submitted envelopes; socket completion does not reduce these.
    pub submitted: Counts,
    /// Cumulative consumed envelopes, used for duplicate/stale feedback detection.
    pub consumed: Counts,
    /// Original cleanup deadline; feedback cannot extend it.
    pub deadline: Option<UnixMillis>,
}

impl From<Delivery> for Snapshot {
    fn from(delivery: Delivery) -> Self {
        Self {
            phase: delivery.phase(),
            usage: delivery.usage(),
            submitted: delivery.submitted,
            consumed: delivery.consumed,
            deadline: delivery.deadline(),
        }
    }
}
