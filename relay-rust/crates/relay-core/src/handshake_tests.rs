extern crate std;

use core::time::Duration;
use std::{string::String, vec::Vec};

use serde::Deserialize;

use super::{
    AdmissionRequest, CloseReason, ControlHandshake, FIRST_FRAME_TIMEOUT, Outcome, Phase,
    REGISTRATION_TIMEOUT, STOP_TIMEOUT, Step,
};
use crate::{
    FrameError,
    authentication::AuthenticationError,
    challenge::ChallengeSeed,
    clock::UnixMillis,
    descriptor_upload::DescriptorUpload,
    error_frame::ErrorFrame,
    identifiers::RelayIdentity,
    registration::RegisterInit,
    stop::StopInit,
    test_vectors::{
        Decimal, array, bytes, challenge_seed, relay_identity, session_case, unix_seconds,
    },
};

/// An arbitrary connection start for cases whose challenge bytes are not pinned.
const OPENED_AT: UnixMillis = UnixMillis(1_700_000_000_000);

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct HandshakeVector {
    relay_identity_b64: String,
    challenge_id_b64: String,
    challenge_nonce_b64: String,
    expires_at: Decimal,
    register_init_b64: String,
    register_challenge_b64: String,
    register_proof_b64: String,
    descriptor_upload_b64: String,
    resume_challenge_id_b64: String,
    resume_challenge_nonce_b64: String,
    resume_expires_at: Decimal,
    resume_init_b64: String,
    resume_credential_b64: String,
    resume_challenge_b64: String,
    resume_proof_b64: String,
    stop_challenge_id_b64: String,
    stop_challenge_nonce_b64: String,
    stop_expires_at: Decimal,
    stop_init_b64: String,
    stop_challenge_b64: String,
    stop_proof_b64: String,
    join_b64: String,
}

impl HandshakeVector {
    fn load() -> Self {
        session_case("fresh-relay-registration-proof")
    }

    fn relay(&self) -> RelayIdentity {
        relay_identity(&self.relay_identity_b64)
    }

    fn open(&self) -> ControlHandshake {
        ControlHandshake::open(self.relay(), OPENED_AT)
    }

    fn register_seed(&self) -> ChallengeSeed {
        challenge_seed(&self.challenge_id_b64, &self.challenge_nonce_b64)
    }
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct UploadAuthorizationVector {
    relay_identity_b64: String,
    register_challenge_b64: String,
    cases: Vec<UploadAuthorizationCase>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct UploadAuthorizationCase {
    name: String,
    register_init_b64: String,
    register_proof_b64: String,
    descriptor_upload_b64: String,
    relay_upload_authorized: bool,
}

/// When a phase lasting `timeout` must start for its deadline to fall one
/// millisecond before `expires_at`, so the shared expiry is exactly that
/// deadline rounded up to whole seconds.
fn started_for(expires_at: &str, timeout: Duration) -> UnixMillis {
    let timeout = u64::try_from(timeout.as_millis()).expect("timeouts fit in milliseconds");
    UnixMillis(unix_seconds(expires_at).start().0 - 1 - timeout)
}

#[test]
fn fresh_registration_spends_one_challenge_and_publishes_its_descriptor() {
    let vector = HandshakeVector::load();
    let at = started_for(&vector.expires_at, REGISTRATION_TIMEOUT);
    let init_bytes = bytes(&vector.register_init_b64);
    let init = RegisterInit::decode(&init_bytes).expect("shared WS2R");
    let mut handshake = ControlHandshake::open(vector.relay(), at);

    assert_eq!(
        handshake.receive(&init_bytes, at),
        Step::RequestAdmission(AdmissionRequest::Registration(init))
    );
    assert_eq!(handshake.phase(), Phase::AwaitingAdmission);
    assert_eq!(
        handshake.deadline(),
        Some(at.saturating_add(REGISTRATION_TIMEOUT))
    );
    assert_eq!(
        handshake.admit(vector.register_seed(), at),
        Step::SendChallenge(array(&vector.register_challenge_b64))
    );
    assert_eq!(handshake.phase(), Phase::AwaitingRegistrationProof);
    assert_eq!(
        handshake.receive(&bytes(&vector.register_proof_b64), at),
        Step::AwaitFrame
    );
    assert_eq!(handshake.phase(), Phase::AwaitingDescriptor);

    let upload = bytes(&vector.descriptor_upload_b64);
    match handshake.receive(&upload, at) {
        Step::Complete(Outcome::Publish(descriptor)) => {
            let expected = DescriptorUpload::decode(&upload).expect("shared WS2U");
            assert_eq!(descriptor.object(), expected.object());
            assert_eq!(descriptor.registration().init(), &init);
        }
        step => panic!("the committed upload must publish: {step:?}"),
    }
    assert_eq!(handshake.phase(), Phase::Finished);
    assert_eq!(handshake.deadline(), None);
}

#[test]
fn resume_presents_its_token_before_spending_a_challenge() {
    let vector = HandshakeVector::load();
    let at = started_for(&vector.resume_expires_at, REGISTRATION_TIMEOUT);
    let init_bytes = bytes(&vector.resume_init_b64);
    let init = RegisterInit::decode(&init_bytes).expect("shared WS2R");
    let mut handshake = ControlHandshake::open(vector.relay(), at);

    assert_eq!(handshake.receive(&init_bytes, at), Step::AwaitFrame);
    assert_eq!(handshake.phase(), Phase::AwaitingResumeCredential);
    assert_eq!(
        handshake.receive(&bytes(&vector.resume_credential_b64), at),
        Step::RequestAdmission(AdmissionRequest::Registration(init))
    );
    let seed = challenge_seed(
        &vector.resume_challenge_id_b64,
        &vector.resume_challenge_nonce_b64,
    );
    assert_eq!(
        handshake.admit(seed, at),
        Step::SendChallenge(array(&vector.resume_challenge_b64))
    );
    match handshake.receive(&bytes(&vector.resume_proof_b64), at) {
        Step::Complete(Outcome::Resume(registration)) => assert_eq!(registration.init(), &init),
        step => panic!("the resume proof must complete the resume: {step:?}"),
    }
}

#[test]
fn stop_spends_one_challenge_and_grants_its_authority() {
    let vector = HandshakeVector::load();
    let at = started_for(&vector.stop_expires_at, STOP_TIMEOUT);
    let init_bytes = bytes(&vector.stop_init_b64);
    let init = StopInit::decode(&init_bytes).expect("shared WS2X");
    let mut handshake = ControlHandshake::open(vector.relay(), at);

    assert_eq!(
        handshake.receive(&init_bytes, at),
        Step::RequestAdmission(AdmissionRequest::Stop(init))
    );
    let seed = challenge_seed(
        &vector.stop_challenge_id_b64,
        &vector.stop_challenge_nonce_b64,
    );
    assert_eq!(
        handshake.admit(seed, at),
        Step::SendChallenge(array(&vector.stop_challenge_b64))
    );
    assert_eq!(handshake.phase(), Phase::AwaitingStopProof);
    match handshake.receive(&bytes(&vector.stop_proof_b64), at) {
        Step::Complete(Outcome::Stop(authority)) => assert_eq!(authority.init(), &init),
        step => panic!("the STOP proof must grant STOP authority: {step:?}"),
    }
}

#[test]
fn join_completes_without_sender_authority() {
    let vector = HandshakeVector::load();
    let init = RegisterInit::decode(&bytes(&vector.register_init_b64)).expect("shared WS2R");
    let mut handshake = vector.open();
    assert_eq!(
        handshake.receive(&bytes(&vector.join_b64), OPENED_AT),
        Step::Complete(Outcome::Join(init.route().share_id()))
    );
    assert_eq!(handshake.phase(), Phase::Finished);
}

#[test]
fn handshakes_publish_only_the_committed_descriptor() {
    let vector = HandshakeVector::load();
    let uploads: UploadAuthorizationVector = session_case("relay-descriptor-upload-authorization");
    let relay = relay_identity(&uploads.relay_identity_b64);
    let at = started_for(&vector.expires_at, REGISTRATION_TIMEOUT);
    for case in uploads.cases {
        let mut handshake = ControlHandshake::open(relay, at);
        let name = &case.name;
        assert!(
            matches!(
                handshake.receive(&bytes(&case.register_init_b64), at),
                Step::RequestAdmission(_)
            ),
            "{name}"
        );
        assert_eq!(
            handshake.admit(vector.register_seed(), at),
            Step::SendChallenge(array(&uploads.register_challenge_b64)),
            "{name}"
        );
        assert_eq!(
            handshake.receive(&bytes(&case.register_proof_b64), at),
            Step::AwaitFrame,
            "{name}"
        );
        match handshake.receive(&bytes(&case.descriptor_upload_b64), at) {
            Step::Complete(Outcome::Publish(_)) => assert!(case.relay_upload_authorized, "{name}"),
            Step::Close(CloseReason::DescriptorMismatch) => {
                assert!(!case.relay_upload_authorized, "{name}");
            }
            step => panic!("{name}: an authenticated upload publishes or mismatches: {step:?}"),
        }
    }
}

#[test]
fn deadlines_bound_every_phase_including_admission() {
    let vector = HandshakeVector::load();
    let first_deadline = OPENED_AT.saturating_add(FIRST_FRAME_TIMEOUT);

    let mut silent = vector.open();
    assert_eq!(silent.deadline(), Some(first_deadline));
    assert!(!silent.expire(UnixMillis(first_deadline.0 - 1)));
    assert!(silent.expire(first_deadline));
    assert_eq!(silent.phase(), Phase::Finished);
    assert!(!silent.expire(first_deadline));

    let mut late = vector.open();
    assert_eq!(
        late.receive(&bytes(&vector.join_b64), first_deadline),
        Step::Close(CloseReason::Expired)
    );

    // REGISTER_INIT replaces the first-frame budget, and waiting on the
    // lifecycle spends the registration budget.
    let mut slow_admission = vector.open();
    let registered_at = UnixMillis(first_deadline.0 - 1);
    assert!(matches!(
        slow_admission.receive(&bytes(&vector.register_init_b64), registered_at),
        Step::RequestAdmission(_)
    ));
    let registration_deadline = registered_at.saturating_add(REGISTRATION_TIMEOUT);
    assert_eq!(slow_admission.deadline(), Some(registration_deadline));
    assert_eq!(
        slow_admission.admit(vector.register_seed(), registration_deadline),
        Step::Close(CloseReason::Expired)
    );

    let mut slow_proof = vector.open();
    let stop_deadline = OPENED_AT.saturating_add(STOP_TIMEOUT);
    assert!(matches!(
        slow_proof.receive(&bytes(&vector.stop_init_b64), OPENED_AT),
        Step::RequestAdmission(_)
    ));
    assert!(matches!(
        slow_proof.admit(vector.register_seed(), OPENED_AT),
        Step::SendChallenge(_)
    ));
    assert_eq!(
        slow_proof.receive(&bytes(&vector.stop_proof_b64), stop_deadline),
        Step::Close(CloseReason::Expired)
    );
}

#[test]
fn a_rejected_proof_spends_the_only_challenge() {
    let vector = HandshakeVector::load();
    let proof = bytes(&vector.register_proof_b64);
    let mut forged = proof.clone();
    *forged.last_mut().expect("proofs carry a signature") ^= 1;
    let mut handshake = vector.open();

    assert!(matches!(
        handshake.receive(&bytes(&vector.register_init_b64), OPENED_AT),
        Step::RequestAdmission(_)
    ));
    assert!(matches!(
        handshake.admit(vector.register_seed(), OPENED_AT),
        Step::SendChallenge(_)
    ));
    assert_eq!(
        handshake.receive(&forged, OPENED_AT),
        Step::Close(CloseReason::Unauthenticated(
            AuthenticationError::InvalidSignature
        ))
    );
    assert_eq!(
        handshake.receive(&proof, OPENED_AT),
        Step::Close(CloseReason::UnexpectedFrame)
    );
}

#[test]
fn a_foreign_resume_token_is_refused_before_admission() {
    let vector = HandshakeVector::load();
    let mut credential = bytes(&vector.resume_credential_b64);
    *credential.last_mut().expect("credentials carry a token") ^= 1;
    let mut handshake = vector.open();

    assert_eq!(
        handshake.receive(&bytes(&vector.resume_init_b64), OPENED_AT),
        Step::AwaitFrame
    );
    assert_eq!(
        handshake.receive(&credential, OPENED_AT),
        Step::Refuse(ErrorFrame::InvalidProof)
    );
    assert_eq!(handshake.phase(), Phase::Finished);
}

#[test]
fn a_stop_for_another_relay_never_reaches_admission() {
    let vector = HandshakeVector::load();
    let other_relay = RelayIdentity::new([0x5a; 32]).expect("nonzero relay identity");
    assert_ne!(other_relay, vector.relay());
    let mut handshake = ControlHandshake::open(other_relay, OPENED_AT);
    assert_eq!(
        handshake.receive(&bytes(&vector.stop_init_b64), OPENED_AT),
        Step::Close(CloseReason::MisaddressedStop)
    );
}

#[test]
fn frames_and_admissions_out_of_turn_close_the_handshake() {
    let vector = HandshakeVector::load();
    let init = bytes(&vector.register_init_b64);

    let mut proof_first = vector.open();
    assert_eq!(
        proof_first.receive(&bytes(&vector.register_proof_b64), OPENED_AT),
        Step::Close(CloseReason::Malformed(FrameError::Header))
    );

    let mut truncated = vector.open();
    assert_eq!(
        truncated.receive(&init[..init.len() - 1], OPENED_AT),
        Step::Close(CloseReason::Malformed(FrameError::Length))
    );

    let mut eager = vector.open();
    assert!(matches!(
        eager.receive(&init, OPENED_AT),
        Step::RequestAdmission(_)
    ));
    assert_eq!(
        eager.receive(&bytes(&vector.register_proof_b64), OPENED_AT),
        Step::Close(CloseReason::UnexpectedFrame)
    );

    let mut unasked = vector.open();
    assert_eq!(
        unasked.admit(vector.register_seed(), OPENED_AT),
        Step::Close(CloseReason::NoPendingAdmission)
    );
    assert_eq!(
        unasked.admit(vector.register_seed(), OPENED_AT),
        Step::Close(CloseReason::NoPendingAdmission)
    );

    let mut skipped_proof = vector.open();
    assert!(matches!(
        skipped_proof.receive(&init, OPENED_AT),
        Step::RequestAdmission(_)
    ));
    assert!(matches!(
        skipped_proof.admit(vector.register_seed(), OPENED_AT),
        Step::SendChallenge(_)
    ));
    assert_eq!(
        skipped_proof.receive(&bytes(&vector.descriptor_upload_b64), OPENED_AT),
        Step::Close(CloseReason::Malformed(FrameError::Header))
    );
}

#[test]
fn seeds_reject_the_reserved_zero_values() {
    assert!(ChallengeSeed::new([0; 16], [1; 32]).is_none());
    assert!(ChallengeSeed::new([1; 16], [0; 32]).is_none());
}
