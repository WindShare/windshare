//! Events and host effects of fresh registration.

use crate::{
    clock::UnixMillis,
    error_frame::{ErrorFrame, RetryAfter},
    identifiers::ShareId,
    registration::RegisterInit,
};

use super::{
    ConnectionRef, Publication, PublishRequest, PublishedRegistration, RegistrationId, Reservation,
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
        id: RegistrationId,
        /// Terminal evidence, or an explicit uncertain result.
        result: PublicationResult,
    },
    /// Releases a pre-publication attempt, or fences its in-flight transaction.
    /// Published sender disconnect/RESUME/STOP belongs to the next lifecycle stage.
    Cancel {
        /// Never look this up again by connection ID after asynchronous work.
        id: RegistrationId,
        /// Structured reason recorded in observations.
        reason: Cancellation,
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
    /// Fresh registration is the only operation this stage admits.
    UnsupportedMode,
    /// The original handshake deadline has passed.
    Expired,
    /// Another attempt or published sender owns the share.
    Occupied,
    /// A transaction needs authoritative reconciliation before another attempt.
    PublicationUncertain,
    /// The incarnation exhausted its non-reusable attempt identities.
    SequenceExhausted,
    /// The reservation or transaction is no longer the current operation.
    StaleOperation,
    /// A receiver arrived before a share was reserved.
    NotFound,
    /// A receiver must wait for publication.
    Starting,
    /// A receiver must wait for the restored route's sender to reattach.
    SenderDetached,
    /// The share instance was permanently stopped.
    Stopped,
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
            Self::PublicationUncertain | Self::SequenceExhausted => {
                Some(ErrorFrame::Admission(RETRY))
            }
            Self::Starting | Self::SenderDetached => Some(ErrorFrame::Starting(RETRY)),
            Self::Stopped => Some(ErrorFrame::Stopped),
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
    /// Replace the unresolved record with `record()`, then send REGISTERED;
    /// JOIN readiness follows. If that write fails or is uncertain, close the
    /// share's connections and restore it from storage.
    Registered(PublishedRegistration),
    /// Lifecycle permits receiver admission using these exact host resources.
    JoinReady(PublishedRegistration),
    /// The attempt never reached publication; close its connection only if
    /// this exact attempt still owns the host's connection resources. Storage
    /// needs no write: a reservation stores nothing, and an abort already
    /// proved its unresolved record gone.
    Released(RegistrationId),
    /// Keep the route hidden, close this attempt's connection and reconcile
    /// only its resources and unresolved record. Report Aborted only after all
    /// work is quiescent and neither remains. Never retry publication.
    Reconcile(RegistrationId),
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
    Released(RegistrationId),
    /// Host work may already have committed; close and reconcile this attempt.
    Reconcile(RegistrationId),
}

/// One event may expire an older attempt before deciding on the current input.
#[derive(Debug, Eq, PartialEq)]
#[must_use = "execute both the expiration and the requested action"]
pub struct Effects<'f> {
    /// Timer cleanup is explicit even when a peer request discovers expiry.
    pub expiration: Option<Expiration>,
    /// Decision for the input itself.
    pub action: Action<'f>,
}
