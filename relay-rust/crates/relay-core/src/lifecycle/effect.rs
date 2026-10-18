//! Events and host effects of share ownership and permanent retirement.

use crate::{
    clock::UnixMillis,
    error_frame::{ErrorFrame, RetryAfter},
    identifiers::{RelaySessionId, ShareId},
    join::JoinRequest,
    registration::RegisterInit,
    sessions::Revocation,
    stop::StopAuthority,
};

use super::{
    ConnectionRef, GraceRoute, Joined, OperationId, OwnershipCommit, Publication,
    PublishedRegistration, PublishedRoute, RegistrationCommit, Reservation, ResumeAttempt,
    Resumption, StopAcknowledgement, StopResult, StopWork,
};

/// Why an unpublished registration must no longer become visible.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Cancellation {
    /// Proof, descriptor, or handshake processing failed.
    HandshakeFailed,
    /// The owning connection closed.
    Disconnected,
    /// Host preparation cannot continue.
    HostFailed,
}

/// Input serialized by the host's single-share actor.
#[derive(Debug, Eq, PartialEq)]
pub enum Event<'f> {
    /// Handles a fresh `AdmissionRequest::Registration`. Use the deadline from
    /// the requesting handshake; a resume is deliberately refused here.
    Reserve {
        /// Exact connection lifetime requesting admission.
        connection: ConnectionRef,
        /// Parsed but not yet authenticated registration.
        init: RegisterInit,
        /// Original handshake deadline, never a new timeout.
        deadline: UnixMillis,
    },
    /// Consumes `Outcome::Publish` from the admitted handshake.
    Publish(Publication<'f>),
    /// Confirms the exact ownership record is durable. Failed or uncertain
    /// writes keep the activation pending and retry the same host work.
    OwnershipCommitted(OperationId),
    /// Releases a reservation or detaches a pending activation. A resume handshake that has not yet
    /// requested an ownership commit reserves nothing and needs no cancellation.
    Cancel {
        /// Never look this up again by connection ID after asynchronous work.
        id: OperationId,
        /// Structured reason recorded in observations.
        reason: Cancellation,
    },
    /// Handles a resume `AdmissionRequest::Registration`, whose token already
    /// matched its commitment. Use the deadline from the requesting handshake.
    BeginResume {
        /// Exact connection lifetime requesting the resume.
        connection: ConnectionRef,
        /// Parsed but not yet authenticated registration.
        init: RegisterInit,
        /// Original handshake deadline, never a new timeout.
        deadline: UnixMillis,
    },
    /// Consumes `Outcome::Resume` from the admitted handshake.
    Resume(Resumption),
    /// The owner connection closed, failed or missed liveness without STOP.
    OwnerDisconnected(ConnectionRef),
    /// Consumes an authenticated `Outcome::Stop`, even for an absent route.
    Stop {
        /// Exact connection lifetime to receive STOPPED after completion.
        connection: ConnectionRef,
        /// Only proof authentication can mint this authority.
        authority: StopAuthority,
    },
    /// Reports evidence for the exact requested STOP step. Out-of-order and
    /// stale completions cannot advance the transaction.
    StopFinished {
        /// Copy the work from `Action::Stop`, never look it up after an await.
        work: StopWork,
        /// Uncertain or failed work remains fenced and must be retried.
        result: StopResult,
    },
    /// Inspects readiness without allocating a receiver admission.
    CheckJoin(ShareId),
    /// Consumes a completed receiver handshake after the host reserves capacity.
    Join {
        /// Connection-bound request, including its original absolute deadline.
        request: JoinRequest,
        /// Host-allocated ID, never reused while either connection survives.
        session: RelaySessionId,
    },
    /// The timer/alarm reached the next deadline.
    Tick,
}

/// Reason an input could not advance this lifecycle.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Refusal {
    /// The selected share actor does not own this route.
    WrongShare,
    /// A STOP authority is addressed to another relay.
    WrongRelay,
    /// A sender connection cannot also join its own relay session.
    InvalidReceiver,
    /// The authenticated STOP names another published or stopping instance.
    StopMismatch,
    /// STOP's durable revocation and unsubmitted-queue barrier is incomplete.
    StopPending,
    /// The registration's mode is not the one this event handles.
    UnsupportedMode,
    /// The original sender handshake deadline has passed.
    Expired,
    /// The receiver admission budget elapsed before JOIN could install a session.
    /// Close that receiver; there is no sender challenge to reject.
    JoinExpired,
    /// Another attempt or published sender owns the share.
    Occupied,
    /// The incarnation exhausted its non-reusable attempt identities, or the
    /// route its ownership epochs.
    SequenceExhausted,
    /// The reservation, transaction or owner is no longer current.
    StaleOperation,
    /// No route exists: a receiver arrived before a share was reserved, or an
    /// authenticated resume found nothing to resume.
    NotFound,
    /// A receiver or resume must wait for publication.
    Starting,
    /// A receiver must wait for the grace route's sender to resume.
    SenderDetached,
    /// The share instance was permanently stopped.
    Stopped,
    /// The share holds a registration other than the one the resume names.
    ResumeMismatch,
    /// A publication or another resume replaced the ownership the resume
    /// observed; the sender resumes again.
    ResumeStale,
}

impl Refusal {
    /// Maps lifecycle refusals onto the existing wire contract. Stale host work
    /// is ignored locally and must not reply on a replacement connection.
    #[must_use]
    pub const fn frame(self) -> Option<ErrorFrame> {
        match self {
            Self::WrongShare | Self::NotFound => Some(ErrorFrame::NotFound),
            Self::UnsupportedMode => Some(ErrorFrame::UnsupportedMode),
            Self::InvalidReceiver => Some(ErrorFrame::Malformed),
            Self::Expired => Some(ErrorFrame::ChallengeExpired),
            Self::Occupied => Some(ErrorFrame::AlreadyRegistered),
            Self::SequenceExhausted | Self::StopPending => Some(ErrorFrame::Admission(RETRY)),
            Self::Starting | Self::SenderDetached => Some(ErrorFrame::Starting(RETRY)),
            Self::Stopped => Some(ErrorFrame::Stopped),
            Self::ResumeMismatch | Self::StopMismatch | Self::WrongRelay => {
                Some(ErrorFrame::InvalidProof)
            }
            Self::ResumeStale => Some(ErrorFrame::ResumeStale),
            Self::StaleOperation | Self::JoinExpired => None,
        }
    }
}

// One short retry hint for a receiver waiting on lifecycle work, independent
// of platform throughput or Cloudflare quota limits.
const RETRY: RetryAfter = match RetryAfter::from_millis(250) {
    Some(retry) => retry,
    None => unreachable!(),
};

/// Host action resulting from an input.
#[derive(Debug, Eq, PartialEq)]
#[must_use = "the host must execute every lifecycle action"]
pub enum Action<'f> {
    /// Pass this move-only reservation into the requesting handshake.
    Reserved(Reservation),
    /// Atomically create ownership and descriptor, then confirm the exact
    /// ownership commit. No reply or admission is authorized before confirmation.
    Register(RegistrationCommit<'f>),
    /// Persist this exact ownership change, close its replaced owner, then
    /// report `Event::OwnershipCommitted`. No reply or admission is authorized.
    CommitOwnership(OwnershipCommit),
    /// Final persistence is confirmed and this owner may serve. Send REGISTERED.
    Registered(PublishedRegistration),
    /// Pass this attempt into the requesting handshake.
    ResumeAdmitted(ResumeAttempt),
    /// The owner is gone without STOP: store `record()`, retire the owner's
    /// relay sessions and schedule `deadline()`. If that write fails, a
    /// restore from storage starts grace again.
    Detached(GraceRoute),
    /// Execute this STOP phase and report its exact work through
    /// `Event::StopFinished`. No phase authorizes a reply.
    Stop(StopWork),
    /// Permanent revocation, queue discard and publication quiescence are durable.
    /// Send STOPPED only to this generation; retry recovers a lost response.
    /// Submitted delivery occupancy remains the independent ledger's responsibility.
    Stopped(StopAcknowledgement),
    /// Lifecycle permits receiver admission using these exact host resources.
    JoinReady(PublishedRegistration),
    /// Install this session and deliver the owner's sealed descriptor. Any failed
    /// preparation ends this exact admission through the session state machine.
    Joined(Joined),
    /// The attempt never reached publication; close its connection only if
    /// this exact attempt still owns the host's connection resources. Storage
    /// needs no write because a reservation stores nothing.
    Released(OperationId),
    /// Refuse a peer input, or ignore a stale host callback.
    Refused(Refusal),
    /// No work is needed.
    None,
}

/// Deadline effects can accompany a new request for the same share.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Expiration {
    /// The ownership write missed its handshake deadline. Close the pending
    /// owner and persist this grace after the outstanding ownership write.
    Detached(GraceRoute),
    /// Only a reservation existed; close its connection resources only if
    /// they still belong to this exact attempt.
    Released(OperationId),
    /// The grace route lapsed without a resume: delete only its exact grace
    /// record, never a newer publication or a stopping/stopped record.
    /// If the delete fails, restore the share from storage, which lapses the
    /// stored route again instead of leaving it to block a new publication.
    Lapsed(PublishedRoute),
}

/// One event may expire an older attempt before deciding on the current input.
#[derive(Debug, Eq, PartialEq)]
#[must_use = "apply revocation, execute expiration, then perform the requested action"]
pub struct Effects<'f> {
    /// Apply immediately to sessions of this exact old ownership. Retain the
    /// resulting cleanup work: STOP executes physical retirement only in its
    /// Retire step, after storing intent. Live permit checks already reject old
    /// routes even before the host applies this revocation.
    pub revocation: Option<Revocation>,
    /// Timer cleanup is explicit even when a peer request discovers expiry.
    pub expiration: Option<Expiration>,
    /// Decision for the input itself.
    pub action: Action<'f>,
}
