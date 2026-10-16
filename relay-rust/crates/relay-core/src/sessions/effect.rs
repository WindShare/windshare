//! Session inputs and platform-independent cleanup decisions.

use crate::lifecycle::ConnectionRef;

use super::{ForwardPermit, OpaqueHeader, Revocation, SessionAdmitted, SessionKey};

/// A bounded session input. Opaque payload bytes never enter this state machine.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Event {
    /// The host validated a complete WS2O envelope from this connection.
    Frame {
        /// Authenticated socket lifetime supplying direction.
        source: ConnectionRef,
        /// Fixed routing metadata only.
        header: OpaqueHeader,
    },
    /// WS2M from the current sender, after an end-to-end receiver handshake.
    Admit {
        /// Exact connection lifetime delivering the control.
        sender: ConnectionRef,
        /// Strictly decoded WS2M.
        frame: SessionAdmitted,
    },
    /// End only this admission, including asynchronous transport failures.
    End {
        /// Copy this before awaiting; never re-resolve by wire ID afterwards.
        session: SessionKey,
        /// Exact participant whose work ended.
        participant: ConnectionRef,
        /// Diagnostic cause; all cases revoke both directions.
        reason: EndReason,
    },
    /// Apply a lifecycle's exact old-owner cleanup instruction.
    Revoke(Revocation),
    /// The admission timer/alarm fired; active sessions have no idle deadline.
    Tick,
}

/// Host-local causes of ending a single session.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum EndReason {
    /// The receiver connection ended. Sender loss belongs to the share lifecycle.
    ReceiverDisconnected,
    /// A participant explicitly cancelled this session.
    Cancelled,
    /// Preparation or forwarding failed for this session.
    HostFailed,
}

/// Stable retirement vocabulary for both platform adapters.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RetirementReason {
    /// The original provisional deadline passed without sender admission.
    AdmissionExpired,
    /// Authenticated STOP fenced the share, even before persistence completed.
    Stopped,
    /// A resume or actor restoration replaced the admitted ownership.
    OwnerReplaced,
    /// The sender disconnected and its route entered grace.
    OwnerDisconnected,
    /// No published owner remains.
    OwnerUnavailable,
    /// Session-local cancellation or transport failure.
    Ended(EndReason),
}

/// A cleanup obligation for exactly one admission.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[must_use = "discard unsent frames and retire exactly these session resources"]
pub struct Retirement {
    /// Both exact connection generations and the never-reused admission.
    pub session: SessionKey,
    /// Why forwarding was revoked.
    pub reason: RetirementReason,
}

/// Why an input is not authorized to affect this session.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Refusal {
    /// The lifecycle belongs to another relay/share.
    ForeignLifecycle,
    /// The envelope or callback names another admission.
    StaleSession,
    /// The socket does not own the requested role in this session.
    WrongParticipant,
    /// A sender cannot confirm admission before any receiver frame.
    ReceiverNotObserved,
    /// Cleanup names another sender ownership.
    StaleRevocation,
}

/// Decision for the input itself.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Action {
    /// Route authorization only: the host still enforces framing and flow control,
    /// then rechecks this permit immediately before submitting a queued frame.
    Forward(ForwardPermit),
    /// The sender retained the session; repeated admission is harmless.
    Active,
    /// An exact former participant's late input is safe to discard locally.
    Discard(RetirementReason),
    /// Reject without affecting sibling sessions or a replacement generation.
    Refused(Refusal),
    /// No additional work.
    None,
}

/// Retirement precedes any response or new forwarding decision.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[must_use = "execute session retirement before the requested action"]
pub struct Effects {
    /// Revoke queued permits and discard unsent frames, then order retirement
    /// controls after already-submitted data. Close only this receiver if needed;
    /// never close a shared sender for a session-local failure.
    ///
    /// Retirement does not release submitted bytes or settle connection credit.
    /// Apply this retirement to each destination's flow ledger; host confirmation
    /// of queue discard releases unsent occupancy, while consumption/settlement
    /// or confirmed connection closure releases submitted occupancy.
    /// STOP defers physical cleanup to its Retire step after persisting intent.
    pub retirement: Option<Retirement>,
    /// Outcome of this input.
    pub action: Action,
}
