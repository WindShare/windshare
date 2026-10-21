//! Shared wire classification; hosts keep payload ownership and policy I/O.
use crate::{
    access::RegistrationRequest,
    descriptor_upload::MAGIC as DESCRIPTOR_UPLOAD_MAGIC,
    flow::{DeliveryConsumed, SettlementAck},
    heartbeat::ConnectionProbe,
    join::JOIN_MAGIC,
    lifecycle::ConnectionRef,
    registration::{REGISTER_INIT_MAGIC, REGISTER_PROOF_MAGIC, RESUME_CREDENTIAL_MAGIC},
    sessions::{OpaqueHeader, SessionAdmitted},
    share::Input,
    stop::{STOP_INIT_MAGIC, STOP_PROOF_MAGIC},
};

/// Recognizing a header does not validate its layout, authority or protocol phase.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum FrameKind {
    Opaque,
    RegisterInit,
    RegisterProof,
    ResumeCredential,
    DescriptorUpload,
    StopInit,
    StopProof,
    Join,
    SessionAdmitted,
    ConnectionProbe,
    DeliveryConsumed,
    SettlementAck,
    Unsupported,
}

impl FrameKind {
    #[must_use]
    pub const fn classify(bytes: &[u8]) -> Self {
        match bytes.first_chunk::<4>().copied() {
            Some(OpaqueHeader::MAGIC) => Self::Opaque,
            Some(REGISTER_INIT_MAGIC) => Self::RegisterInit,
            Some(REGISTER_PROOF_MAGIC) => Self::RegisterProof,
            Some(RESUME_CREDENTIAL_MAGIC) => Self::ResumeCredential,
            Some(DESCRIPTOR_UPLOAD_MAGIC) => Self::DescriptorUpload,
            Some(STOP_INIT_MAGIC) => Self::StopInit,
            Some(STOP_PROOF_MAGIC) => Self::StopProof,
            Some(JOIN_MAGIC) => Self::Join,
            Some(SessionAdmitted::MAGIC) => Self::SessionAdmitted,
            Some(ConnectionProbe::MAGIC) => Self::ConnectionProbe,
            Some(DeliveryConsumed::MAGIC) => Self::DeliveryConsumed,
            Some(SettlementAck::MAGIC) => Self::SettlementAck,
            _ => Self::Unsupported,
        }
    }
}

/// Policy resolution is required only for a validated fresh registration.
/// Malformed known frames still reach the engine's phase-specific validation.
#[derive(Debug)]
pub enum Control<'a> {
    Authorize(RegistrationRequest),
    Ready(Input<'a>),
}

impl<'a> Control<'a> {
    #[must_use]
    pub fn inspect(connection: ConnectionRef, frame: &'a [u8]) -> Self {
        match FrameKind::classify(frame) {
            FrameKind::Opaque | FrameKind::Unsupported => {
                Self::Ready(Input::Unsupported(connection))
            }
            FrameKind::RegisterInit => RegistrationRequest::from_frame(connection, frame).map_or(
                Self::Ready(Input::Control { connection, frame }),
                Self::Authorize,
            ),
            _ => Self::Ready(Input::Control { connection, frame }),
        }
    }
}

#[cfg(test)]
#[path = "ingress_tests.rs"]
mod tests;
