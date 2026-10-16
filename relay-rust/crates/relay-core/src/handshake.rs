//! Per-connection control handshake: from a connection's first frame to the
//! authority, refusal or close that ends its control phase.
//!
//! Every sender proof answers a challenge issued on the same connection, and
//! no proof frame names its challenge, so the handshake owns its single
//! outstanding challenge by value. Outstanding challenges are then bounded by
//! admitted connections and live beside their socket; a process-wide ledger
//! would be redundant natively and cannot exist across Cloudflare's per-share
//! objects.
//!
//! The handshake performs no I/O. Hosts feed it frames and the time, carry
//! out each [`Step`], and answer [`Step::RequestAdmission`] from their share
//! lifecycle before a challenge is spent.

use core::{mem, time::Duration};

use crate::{
    authentication::AuthenticationError,
    challenge::{Challenge, ChallengePurpose, ChallengeSeed},
    clock::UnixMillis,
    descriptor_upload::{DescriptorDigestMismatch, DescriptorUpload},
    error_frame::ErrorFrame,
    identifiers::{RelayIdentity, ShareId},
    join::{JOIN_MAGIC, Join},
    lifecycle::{ConnectionRef, Publication, RegistrationId, Reservation},
    registration::{
        FreshRegistration, REGISTER_INIT_MAGIC, RegisterInit, RegisterProof, RegistrationAuthority,
        RegistrationMode, ResumeCredential, ResumeRegistration, authenticate_registration,
    },
    stop::{STOP_INIT_MAGIC, StopAuthority, StopInit, StopProof, authenticate_stop},
    wire::FrameError,
};

/// How long a new connection may stay silent before naming its role.
const FIRST_FRAME_TIMEOUT: Duration = Duration::from_secs(10);

/// How long a registration may take from `REGISTER_INIT` until it can be
/// published. Short because an admitted registration reserves its route,
/// which holds back receiver joins and competing registrations.
const REGISTRATION_TIMEOUT: Duration = Duration::from_secs(5);

/// How long a STOP may take from `STOP_INIT` until its proof arrives. A STOP
/// reserves nothing before it authenticates, so it keeps the connection's full
/// first-frame budget for a sender shutting down on a congested uplink.
const STOP_TIMEOUT: Duration = Duration::from_secs(10);

/// What the handshake is waiting for, as structured traces label it.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Phase {
    /// The connection has not yet named its role.
    AwaitingFirstFrame,
    /// A resume waits for the token its registration committed.
    AwaitingResumeCredential,
    /// The host's share lifecycle decides whether to spend a challenge.
    AwaitingAdmission,
    /// A registration or resume waits for the proof answering its challenge.
    AwaitingRegistrationProof,
    /// An authenticated fresh registration waits for its committed descriptor.
    AwaitingDescriptor,
    /// A STOP waits for the proof answering its challenge.
    AwaitingStopProof,
    /// The handshake ended and accepts no further input.
    Finished,
}

/// An operation the host's share lifecycle must admit before the handshake
/// issues its challenge.
///
/// Admission comes first so a share that cannot take the operation, such as
/// an occupied route or a stopped instance, refuses without spending one.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AdmissionRequest {
    /// A fresh registration, or a resume whose token matches its commitment.
    Registration(RegisterInit),
    /// A STOP addressed to this relay.
    Stop(StopInit),
}

/// The host's decision for the exact admission request.
///
/// Fresh registration requires a lifecycle reservation; RESUME and STOP retain
/// their independent admission paths until their lifecycle stages are implemented.
#[derive(Debug, Eq, PartialEq)]
#[expect(
    clippy::large_enum_variant,
    reason = "bounded admission capabilities stay inline in this allocation-free kernel"
)]
pub enum Admission {
    /// Consume the share's reservation for this connection and deadline.
    Fresh(Reservation),
    /// The host admitted recovery of an existing registration.
    Resume,
    /// The host admitted a stop handshake before its sender proof.
    Stop,
}

/// Authority or intent a completed handshake hands to the host.
#[derive(Debug, Eq, PartialEq)]
pub enum Outcome<'f> {
    /// A fresh registration proved sender authority and uploaded the
    /// descriptor it committed; the bytes borrow the upload frame.
    Publish(Publication<'f>),
    /// A resume proved sender authority over the route it names.
    Resume(ResumeRegistration),
    /// A STOP proved sender authority to retire its share instance.
    Stop(StopAuthority),
    /// A receiver asked to join a share, which needs no sender authority.
    Join(ShareId),
}

/// Why a handshake closes its connection without replying.
///
/// Peers learn only that the connection closed, so the variants serve traces
/// and never become a key-, challenge- or route-validity oracle.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CloseReason {
    /// The phase deadline passed before the peer or the host acted.
    Expired,
    /// The frame does not decode as the frame this phase expects.
    Malformed(FrameError),
    /// A frame arrived while the handshake accepts none.
    UnexpectedFrame,
    /// The host admitted an operation the handshake was not waiting on.
    NoPendingAdmission,
    /// The host's grant names another role, registration, relay, connection or deadline.
    AdmissionMismatch,
    /// A STOP named another relay, so it never spends a challenge.
    MisaddressedStop,
    /// The sender proof did not grant authority.
    Unauthenticated(AuthenticationError),
    /// The uploaded descriptor is not the one the registration committed.
    DescriptorMismatch,
}

impl From<FrameError> for CloseReason {
    fn from(error: FrameError) -> Self {
        Self::Malformed(error)
    }
}

impl From<AuthenticationError> for CloseReason {
    fn from(error: AuthenticationError) -> Self {
        Self::Unauthenticated(error)
    }
}

impl From<DescriptorDigestMismatch> for CloseReason {
    fn from(_: DescriptorDigestMismatch) -> Self {
        Self::DescriptorMismatch
    }
}

/// The action a host takes next.
#[derive(Debug, Eq, PartialEq)]
#[must_use = "every step names an action the host must carry out"]
pub enum Step<'f> {
    /// Wait for the peer's next frame.
    AwaitFrame,
    /// Ask the share lifecycle to admit this operation, then either call
    /// [`ControlHandshake::admit`] or send the lifecycle's refusal and close.
    RequestAdmission(AdmissionRequest),
    /// Send this `CHALLENGE` frame and wait for the proof.
    SendChallenge([u8; Challenge::ENCODED_BYTES]),
    /// The handshake succeeded; the connection continues with this outcome.
    Complete(Outcome<'f>),
    /// Send this refusal, then close.
    Refuse(ErrorFrame),
    /// Close without replying.
    Close(CloseReason),
}

/// One connection's control phase, from its first frame to its outcome.
#[derive(Debug)]
pub struct ControlHandshake {
    relay: RelayIdentity,
    connection: ConnectionRef,
    progress: Progress,
}

#[derive(Debug)]
#[expect(
    clippy::large_enum_variant,
    reason = "a pending handshake owns bounded metadata without requiring an allocator"
)]
enum Progress {
    Pending {
        awaiting: Awaiting,
        deadline: UnixMillis,
    },
    Finished,
}

/// The input a pending handshake waits for, with everything it carried so far.
#[derive(Debug)]
enum Awaiting {
    FirstFrame,
    ResumeCredential(RegisterInit),
    Admission(AdmissionRequest),
    RegistrationProof {
        admission: RegistrationAdmission,
        challenge: Challenge,
    },
    Descriptor {
        registration: FreshRegistration,
        id: RegistrationId,
    },
    StopProof {
        init: StopInit,
        challenge: Challenge,
    },
}

#[derive(Debug)]
enum RegistrationAdmission {
    Fresh(Reservation),
    Resume(RegisterInit),
}

impl RegistrationAdmission {
    const fn init(&self) -> RegisterInit {
        match self {
            Self::Fresh(reservation) => *reservation.init(),
            Self::Resume(init) => *init,
        }
    }
}

impl ControlHandshake {
    /// Starts the handshake of a connection `relay` accepted at `now`.
    #[must_use]
    pub fn open(relay: RelayIdentity, connection: ConnectionRef, now: UnixMillis) -> Self {
        Self {
            relay,
            connection,
            progress: Progress::Pending {
                awaiting: Awaiting::FirstFrame,
                deadline: now.saturating_add(FIRST_FRAME_TIMEOUT),
            },
        }
    }

    /// Returns what the handshake is waiting for.
    #[must_use]
    pub const fn phase(&self) -> Phase {
        let Progress::Pending { awaiting, .. } = &self.progress else {
            return Phase::Finished;
        };
        match awaiting {
            Awaiting::FirstFrame => Phase::AwaitingFirstFrame,
            Awaiting::ResumeCredential(_) => Phase::AwaitingResumeCredential,
            Awaiting::Admission(_) => Phase::AwaitingAdmission,
            Awaiting::RegistrationProof { .. } => Phase::AwaitingRegistrationProof,
            Awaiting::Descriptor { .. } => Phase::AwaitingDescriptor,
            Awaiting::StopProof { .. } => Phase::AwaitingStopProof,
        }
    }

    /// Returns the absolute instant at which the current phase expires, or
    /// `None` once the handshake finished.
    #[must_use]
    pub const fn deadline(&self) -> Option<UnixMillis> {
        match &self.progress {
            Progress::Pending { deadline, .. } => Some(*deadline),
            Progress::Finished => None,
        }
    }

    /// Ends the handshake if its deadline has passed at `now`, and reports
    /// whether it did so; the host then closes the connection.
    ///
    /// Hosts call this from their timer or alarm. [`Self::receive`] and
    /// [`Self::admit`] check the deadline themselves, so a host that wakes late
    /// never acts on an expired phase.
    #[must_use]
    pub fn expire(&mut self, now: UnixMillis) -> bool {
        match self.progress {
            Progress::Pending { deadline, .. } if now >= deadline => {
                self.progress = Progress::Finished;
                true
            }
            _ => false,
        }
    }

    /// Consumes one frame the peer sent, observed at `now`.
    ///
    /// Any frame other than the one the current phase expects ends the
    /// handshake, so a peer can never skip, reorder or repeat a step.
    pub fn receive<'f>(&mut self, frame: &'f [u8], now: UnixMillis) -> Step<'f> {
        let (awaiting, deadline) = match self.take_pending(now, CloseReason::UnexpectedFrame) {
            Ok(pending) => pending,
            Err(reason) => return Step::Close(reason),
        };
        let step = match awaiting {
            Awaiting::FirstFrame => self.first_frame(frame, now),
            Awaiting::ResumeCredential(init) => self.resume_credential(init, frame, deadline),
            Awaiting::Admission(_) => Err(CloseReason::UnexpectedFrame),
            Awaiting::RegistrationProof {
                admission,
                challenge,
            } => self.registration_proof(admission, challenge, frame, deadline, now),
            Awaiting::Descriptor { registration, id } => {
                Self::descriptor(registration, id, deadline, frame)
            }
            Awaiting::StopProof { init, challenge } => self.stop_proof(init, challenge, frame, now),
        };
        step.unwrap_or_else(Step::Close)
    }

    /// Issues the challenge for the operation the host's lifecycle admitted.
    ///
    /// The host draws `seed` from a cryptographic random source. Admission
    /// counts against the phase deadline, so a slow lifecycle decision cannot
    /// extend the handshake.
    pub fn admit(
        &mut self,
        admission: Admission,
        seed: ChallengeSeed,
        now: UnixMillis,
    ) -> Step<'static> {
        let (awaiting, deadline) = match self.take_pending(now, CloseReason::NoPendingAdmission) {
            Ok(pending) => pending,
            Err(reason) => return Step::Close(reason),
        };
        let Awaiting::Admission(request) = awaiting else {
            return Step::Close(CloseReason::NoPendingAdmission);
        };
        let (awaiting, challenge) = match (request, admission) {
            (AdmissionRequest::Registration(init), Admission::Fresh(reservation))
                if init.mode() == RegistrationMode::Fresh
                    && reservation.init() == &init
                    && reservation.id().relay() == self.relay
                    && reservation.id().connection() == self.connection
                    && reservation.deadline() == deadline =>
            {
                let challenge = Challenge::issue(ChallengePurpose::Register, seed, deadline);
                let encoded = challenge.encode();
                (
                    Awaiting::RegistrationProof {
                        admission: RegistrationAdmission::Fresh(reservation),
                        challenge,
                    },
                    encoded,
                )
            }
            (AdmissionRequest::Registration(init), Admission::Resume)
                if init.mode() == RegistrationMode::Resume =>
            {
                let challenge = Challenge::issue(ChallengePurpose::Resume, seed, deadline);
                let encoded = challenge.encode();
                (
                    Awaiting::RegistrationProof {
                        admission: RegistrationAdmission::Resume(init),
                        challenge,
                    },
                    encoded,
                )
            }
            (AdmissionRequest::Stop(init), Admission::Stop) => {
                let challenge = Challenge::issue(ChallengePurpose::Stop, seed, deadline);
                let encoded = challenge.encode();
                (Awaiting::StopProof { init, challenge }, encoded)
            }
            _ => return Step::Close(CloseReason::AdmissionMismatch),
        };
        self.continue_with(awaiting, deadline);
        Step::SendChallenge(challenge)
    }

    /// Takes the pending input and leaves the handshake finished, so any path
    /// that does not install a successor fails closed.
    fn take_pending(
        &mut self,
        now: UnixMillis,
        when_finished: CloseReason,
    ) -> Result<(Awaiting, UnixMillis), CloseReason> {
        match mem::replace(&mut self.progress, Progress::Finished) {
            Progress::Finished => Err(when_finished),
            Progress::Pending { deadline, .. } if now >= deadline => Err(CloseReason::Expired),
            Progress::Pending { awaiting, deadline } => Ok((awaiting, deadline)),
        }
    }

    const fn continue_with(&mut self, awaiting: Awaiting, deadline: UnixMillis) {
        self.progress = Progress::Pending { awaiting, deadline };
    }

    const fn request_admission(
        &mut self,
        request: AdmissionRequest,
        deadline: UnixMillis,
    ) -> Step<'static> {
        self.continue_with(Awaiting::Admission(request), deadline);
        Step::RequestAdmission(request)
    }

    fn first_frame<'f>(
        &mut self,
        frame: &'f [u8],
        now: UnixMillis,
    ) -> Result<Step<'f>, CloseReason> {
        match frame.first_chunk().copied() {
            Some(REGISTER_INIT_MAGIC) => Ok(self.registration(RegisterInit::decode(frame)?, now)),
            Some(STOP_INIT_MAGIC) => self.stop(StopInit::decode(frame)?, now),
            Some(JOIN_MAGIC) => Ok(Step::Complete(Outcome::Join(
                Join::decode(frame)?.share_id(),
            ))),
            _ => Err(CloseReason::Malformed(FrameError::Header)),
        }
    }

    fn registration(&mut self, init: RegisterInit, now: UnixMillis) -> Step<'static> {
        let deadline = now.saturating_add(REGISTRATION_TIMEOUT);
        match init.mode() {
            RegistrationMode::Fresh => {
                self.request_admission(AdmissionRequest::Registration(init), deadline)
            }
            RegistrationMode::Resume => {
                self.continue_with(Awaiting::ResumeCredential(init), deadline);
                Step::AwaitFrame
            }
        }
    }

    fn stop(&mut self, init: StopInit, now: UnixMillis) -> Result<Step<'static>, CloseReason> {
        // Authentication checks the relay again; refusing here first keeps a
        // misaddressed STOP from reaching admission or spending a challenge.
        if init.relay() != self.relay {
            return Err(CloseReason::MisaddressedStop);
        }
        let deadline = now.saturating_add(STOP_TIMEOUT);
        Ok(self.request_admission(AdmissionRequest::Stop(init), deadline))
    }

    fn resume_credential(
        &mut self,
        init: RegisterInit,
        frame: &[u8],
        deadline: UnixMillis,
    ) -> Result<Step<'static>, CloseReason> {
        let credential = ResumeCredential::decode(frame)?;
        // Both values come from this unauthenticated peer, so neither a
        // variable-time comparison nor the explicit refusal reveals anything
        // it did not send; no challenge has been spent yet.
        if credential.token().hash() != init.resume_token_hash() {
            return Ok(Step::Refuse(ErrorFrame::InvalidProof));
        }
        Ok(self.request_admission(AdmissionRequest::Registration(init), deadline))
    }

    fn registration_proof(
        &mut self,
        admission: RegistrationAdmission,
        challenge: Challenge,
        frame: &[u8],
        deadline: UnixMillis,
        now: UnixMillis,
    ) -> Result<Step<'static>, CloseReason> {
        let proof = RegisterProof::decode(frame)?;
        let authority =
            authenticate_registration(admission.init(), challenge, &self.relay, &proof, now)?;
        match (admission, authority) {
            (
                RegistrationAdmission::Fresh(reservation),
                RegistrationAuthority::Fresh(registration),
            ) => {
                self.continue_with(
                    Awaiting::Descriptor {
                        registration,
                        id: reservation.id(),
                    },
                    deadline,
                );
                Ok(Step::AwaitFrame)
            }
            (RegistrationAdmission::Resume(_), RegistrationAuthority::Resume(registration)) => {
                Ok(Step::Complete(Outcome::Resume(registration)))
            }
            _ => Err(CloseReason::AdmissionMismatch),
        }
    }

    fn descriptor(
        registration: FreshRegistration,
        id: RegistrationId,
        deadline: UnixMillis,
        frame: &[u8],
    ) -> Result<Step<'_>, CloseReason> {
        let descriptor = registration.authorize_descriptor(DescriptorUpload::decode(frame)?)?;
        Ok(Step::Complete(Outcome::Publish(Publication::new(
            id, deadline, descriptor,
        ))))
    }

    fn stop_proof(
        &self,
        init: StopInit,
        challenge: Challenge,
        frame: &[u8],
        now: UnixMillis,
    ) -> Result<Step<'static>, CloseReason> {
        let proof = StopProof::decode(frame)?;
        let authority = authenticate_stop(init, challenge, &self.relay, &proof, now)?;
        Ok(Step::Complete(Outcome::Stop(authority)))
    }
}

#[cfg(test)]
#[path = "handshake_tests.rs"]
mod tests;
