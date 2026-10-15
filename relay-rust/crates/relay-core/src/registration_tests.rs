extern crate std;

use std::{string::String, vec::Vec};

use serde::Deserialize;

use super::{
    RegisterInit, RegisterProof, Registered, RegistrationAuthority, RegistrationMode,
    ResumeCredential, authenticate_registration,
};
use crate::{
    challenge::ChallengePurpose,
    descriptor_upload::DescriptorUpload,
    test_vectors::{
        Decimal, array, bytes, challenge, last_live_second, relay_identity, session_case,
    },
};

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct RegistrationVector {
    relay_identity_b64: String,
    descriptor_digest_b64: String,
    resume_token_hash_b64: String,
    challenge_id_b64: String,
    challenge_nonce_b64: String,
    expires_at: Decimal,
    register_init_b64: String,
    register_challenge_b64: String,
    preimage_b64: String,
    register_proof_b64: String,
    descriptor_upload_b64: String,
    registered_b64: String,
    resume_challenge_id_b64: String,
    resume_challenge_nonce_b64: String,
    resume_expires_at: Decimal,
    resume_init_b64: String,
    resume_challenge_b64: String,
    resume_preimage_b64: String,
    resume_proof_b64: String,
    resume_credential_b64: String,
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
    preimage_b64: String,
    register_proof_b64: String,
    descriptor_upload_b64: String,
    relay_upload_authorized: bool,
}

fn registration_vector() -> RegistrationVector {
    session_case("fresh-relay-registration-proof")
}

#[test]
fn fresh_registration_reproduces_shared_bytes_and_publishes_its_descriptor() {
    let vector = registration_vector();
    let relay = relay_identity(&vector.relay_identity_b64);
    let init = RegisterInit::decode(&bytes(&vector.register_init_b64)).expect("shared WS2R");
    assert_eq!(init.mode(), RegistrationMode::Fresh);
    assert_eq!(
        init.descriptor_digest().as_bytes(),
        &array(&vector.descriptor_digest_b64)
    );
    assert_eq!(
        init.resume_token_hash().as_bytes(),
        &array(&vector.resume_token_hash_b64)
    );
    assert_eq!(
        Registered::new(&init).encode().as_slice(),
        bytes(&vector.registered_b64)
    );

    let challenge = challenge(
        ChallengePurpose::Register,
        &vector.challenge_id_b64,
        &vector.challenge_nonce_b64,
        &vector.expires_at,
    );
    assert_eq!(
        challenge.encode().as_slice(),
        bytes(&vector.register_challenge_b64)
    );
    assert_eq!(
        init.preimage(&challenge, &relay).written(),
        bytes(&vector.preimage_b64)
    );

    let proof = RegisterProof::decode(&bytes(&vector.register_proof_b64)).expect("shared WS2P");
    let now = last_live_second(&challenge);
    let registration = match authenticate_registration(init, challenge, &relay, &proof, now) {
        Ok(RegistrationAuthority::Fresh(registration)) => registration,
        outcome => panic!("fresh proof must mint fresh authority: {outcome:?}"),
    };
    let upload_bytes = bytes(&vector.descriptor_upload_b64);
    let upload = DescriptorUpload::decode(&upload_bytes).expect("shared WS2U");
    let authorized = registration
        .authorize_descriptor(upload)
        .expect("the committed descriptor is publishable");
    assert_eq!(authorized.object(), upload.object());
    assert_eq!(authorized.registration().init(), &init);
}

#[test]
fn resume_reproduces_shared_bytes_and_presents_its_committed_token() {
    let vector = registration_vector();
    let relay = relay_identity(&vector.relay_identity_b64);
    let init = RegisterInit::decode(&bytes(&vector.resume_init_b64)).expect("shared WS2R");
    assert_eq!(init.mode(), RegistrationMode::Resume);
    let credential =
        ResumeCredential::decode(&bytes(&vector.resume_credential_b64)).expect("shared WS2T");
    assert_eq!(credential.token().hash(), init.resume_token_hash());

    let challenge = challenge(
        ChallengePurpose::Resume,
        &vector.resume_challenge_id_b64,
        &vector.resume_challenge_nonce_b64,
        &vector.resume_expires_at,
    );
    assert_eq!(
        challenge.encode().as_slice(),
        bytes(&vector.resume_challenge_b64)
    );
    assert_eq!(
        init.preimage(&challenge, &relay).written(),
        bytes(&vector.resume_preimage_b64)
    );

    let proof = RegisterProof::decode(&bytes(&vector.resume_proof_b64)).expect("shared WS2P");
    let now = last_live_second(&challenge);
    match authenticate_registration(init, challenge, &relay, &proof, now) {
        Ok(RegistrationAuthority::Resume(registration)) => assert_eq!(registration.init(), &init),
        outcome => panic!("resume proof must mint resume authority: {outcome:?}"),
    }
}

#[test]
fn descriptor_authorization_depends_only_on_the_committed_digest() {
    let registration = registration_vector();
    let uploads: UploadAuthorizationVector = session_case("relay-descriptor-upload-authorization");
    let relay = relay_identity(&uploads.relay_identity_b64);
    for case in uploads.cases {
        let challenge = challenge(
            ChallengePurpose::Register,
            &registration.challenge_id_b64,
            &registration.challenge_nonce_b64,
            &registration.expires_at,
        );
        assert_eq!(
            challenge.encode().as_slice(),
            bytes(&uploads.register_challenge_b64),
            "{}",
            case.name
        );
        let init = RegisterInit::decode(&bytes(&case.register_init_b64)).expect("shared WS2R");
        assert_eq!(
            init.preimage(&challenge, &relay).written(),
            bytes(&case.preimage_b64),
            "{}",
            case.name
        );
        let proof = RegisterProof::decode(&bytes(&case.register_proof_b64)).expect("shared WS2P");
        let now = last_live_second(&challenge);
        let Ok(RegistrationAuthority::Fresh(authority)) =
            authenticate_registration(init, challenge, &relay, &proof, now)
        else {
            panic!("{}: upload vectors carry valid fresh proofs", case.name);
        };
        let upload_bytes = bytes(&case.descriptor_upload_b64);
        let upload = DescriptorUpload::decode(&upload_bytes).expect("shared WS2U");
        assert_eq!(
            authority.authorize_descriptor(upload).is_ok(),
            case.relay_upload_authorized,
            "{}",
            case.name
        );
    }
}
