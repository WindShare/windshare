//! Events and host effects of share ownership and permanent retirement.

use crate::{
    clock::UnixMillis,
    error_frame::{ErrorFrame, RetryAfter},
    identifiers::ShareId,
    registration::RegisterInit,
    stop::StopAuthority,
};

use super::{
    ConnectionRef, GraceRoute, OperationId, Publication, PublishRequest, PublishedRegistration,
    PublishedRoute, Reservation, ResumeAttempt, Resumed, Resumption, StopAcknowledgement,
    StopResult, StopWork,
};

/// Host evidence about one publication transaction.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PublicationResult {
    /// The unresolved record, descriptor bytes and connection preparation all
    /// completed. The route remains hidden until the lifecycle returns
    /// `Action::Registered`.
    Committed,
    /// No stored record, prepared resource, or outstanding write for this
    /// attempt remains. A failed write alone is insufficient evidence of an abort.
    Aborted,
    /// The host cannot establish whether persistence or preparation completed.
    Uncertain,
}

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
    /// Reports the result of the host transaction, including reconciliation.
    PublicationFinished {
        /// Identity copied from `PublishRequest`.
        id: OperationId,
        /// Terminal evidence, or an explicit uncertain result.
        result: PublicationResult,
    },
    /// Releases a pre-publication attempt, or fences its in-flight transaction.
    /// A resume attempt reserves nothing and needs no cancellation.
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
    /// Checks lifecycle readiness; session admission still belongs to the host.
    Join(ShareId),
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
    /// The authenticated STOP names another published or stopping instance.
    StopMismatch,
    /// STOP is not yet durably committed and fully cleaned up.
    StopPending,
    /// The registration's mode is not the one this event handles.
    UnsupportedMode,
    /// The original handshake deadline has passed.
    Expired,
    /// Another attempt or published sender owns the share.
    Occupied,
    /// A transaction needs authoritative reconciliation before another attempt.
    PublicationUncertain,
    /// The incarnation exhausted its non-reusable attempt identities.
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
    /// A publication, another resume or a restore replaced the ownership the
    /// resume observed; the sender resumes again.
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
            Self::Expired => Some(ErrorFrame::ChallengeExpired),
            Self::Occupied => Some(ErrorFrame::AlreadyRegistered),
            Self::PublicationUncertain | Self::SequenceExhausted | Self::StopPending => {
                Some(ErrorFrame::Admission(RETRY))
            }
            Self::Starting | Self::SenderDetached => Some(ErrorFrame::Starting(RETRY)),
            Self::Stopped => Some(ErrorFrame::Stopped),
            Self::ResumeMismatch | Self::StopMismatch | Self::WrongRelay => {
                Some(ErrorFrame::InvalidProof)
            }
            Self::ResumeStale => Some(ErrorFrame::ResumeStale),
            Self::StaleOperation => None,
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
    /// Execute the bounded host transaction before confirming publication.
    Publish(PublishRequest<'f>),
    /// Replace only this operation's unresolved record with `record()`, then
    /// send REGISTERED;
    /// JOIN readiness follows. If that write fails or is uncertain, close the
    /// share's connections and restore it from storage.
    Registered(PublishedRegistration),
    /// Pass this attempt into the requesting handshake.
    ResumeAdmitted(ResumeAttempt),
    /// Replace the stored record with `registration.record()`, close the
    /// replaced owner and its relay sessions, then send REGISTERED; JOIN
    /// readiness follows. If that write fails or is uncertain, close the
    /// share's connections and restore it from storage.
    Resumed(Resumed),
    /// The owner is gone without STOP: store `record()`, retire the owner's
    /// relay sessions and schedule `deadline()`. If that write fails, a
    /// restore from storage starts grace again.
    Detached(GraceRoute),
    /// Execute this STOP phase and report its exact work through
    /// `Event::StopFinished`. No phase authorizes a reply.
    Stop(StopWork),
    /// Persistence and exact cleanup are complete. Send STOPPED only to this
    /// connection generation; a lost response is recovered by authenticated retry.
    Stopped(StopAcknowledgement),
    /// Lifecycle permits receiver admission using these exact host resources.
    JoinReady(PublishedRegistration),
    /// The attempt never reached publication; close its connection only if
    /// this exact attempt still owns the host's connection resources. Storage
    /// needs no write: a reservation stores nothing, and an abort already
    /// proved its unresolved record gone.
    Released(OperationId),
    /// Keep the route hidden, close this attempt's connection and reconcile
    /// only its resources and unresolved record. A stopping or stopped record
    /// must never be removed by this cleanup. Report Aborted only after all
    /// work is quiescent and neither remains. Never retry publication.
    Reconcile(OperationId),
    /// Refuse a peer input, or ignore a stale host callback.
    Refused(Refusal),
    /// No work is needed.
    None,
}

/// Deadline effects can accompany a new request for the same share.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Expiration {
    /// Only a reservation existed; close its connection resources only if
    /// they still belong to this exact attempt.
    Released(OperationId),
    /// Host work may already have committed; close and reconcile this attempt.
    Reconcile(OperationId),
    /// The grace route lapsed without a resume: delete only its exact grace
    /// record, never a newer publication or a stopping/stopped record.
    /// If the delete fails, restore the share from storage, which lapses the
    /// stored route again instead of leaving it to block a new publication.
    Lapsed(PublishedRoute),
}

/// One event may expire an older attempt before deciding on the current input.
#[derive(Debug, Eq, PartialEq)]
#[must_use = "execute the expiration before the requested action"]
pub struct Effects<'f> {
    /// Timer cleanup is explicit even when a peer request discovers expiry.
    pub expiration: Option<Expiration>,
    /// Decision for the input itself.
    pub action: Action<'f>,
}
