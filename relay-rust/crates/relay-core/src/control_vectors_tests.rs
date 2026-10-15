//! Shared refusal vectors: the Go generator derives every hostile frame and
//! proof from canonical bytes, and Go, browser and Rust consumers must agree.

extern crate std;

use std::{collections::BTreeSet, string::String, vec::Vec};

use serde::Deserialize;

use crate::{
    FrameError,
    challenge::ChallengePurpose,
    descriptor_upload::DescriptorUpload,
    registration::{RegisterInit, RegisterProof, ResumeCredential, authenticate_registration},
    stop::{StopInit, StopProof, authenticate_stop},
    test_vectors::{Decimal, bytes, challenge, relay_identity, session_case, unix_seconds},
};

const CLIENT_TO_RELAY: &str = "client-to-relay";
const RELAY_TO_CLIENT: &str = "relay-to-client";

#[derive(Deserialize)]
struct DecodingVector {
    cases: Vec<DecodingCase>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct DecodingCase {
    name: String,
    frame: String,
    direction: String,
    encoded_b64: String,
    accepted: bool,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct ProofVector {
    relay_identity_b64: String,
    cases: Vec<ProofCase>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct ProofCase {
    name: String,
    operation: String,
    init_b64: String,
    challenge_purpose: u8,
    challenge_id_b64: String,
    challenge_nonce_b64: String,
    challenge_expires_at: Decimal,
    proof_b64: String,
    now_unix_seconds: Decimal,
    authenticated: bool,
}

fn decode_client_frame(frame: &str, encoded: &[u8]) -> Result<(), FrameError> {
    match frame {
        "WS2R" => RegisterInit::decode(encoded).map(drop),
        "WS2P" => RegisterProof::decode(encoded).map(drop),
        "WS2T" => ResumeCredential::decode(encoded).map(drop),
        "WS2U" => DescriptorUpload::decode(encoded).map(drop),
        "WS2X" => StopInit::decode(encoded).map(drop),
        "WS2V" => StopProof::decode(encoded).map(drop),
        other => panic!("the relay has no decoder for client frame {other}"),
    }
}

fn challenge_purpose(wire: u8) -> ChallengePurpose {
    match wire {
        0 => ChallengePurpose::Register,
        1 => ChallengePurpose::Resume,
        2 => ChallengePurpose::Stop,
        other => panic!("unknown challenge purpose {other}"),
    }
}

#[test]
fn client_frames_follow_shared_decoding_vectors() {
    let vector: DecodingVector = session_case("relay-control-frame-decoding");
    let mut covered = BTreeSet::new();
    for case in &vector.cases {
        match case.direction.as_str() {
            CLIENT_TO_RELAY => {
                let decoded = decode_client_frame(&case.frame, &bytes(&case.encoded_b64));
                assert_eq!(decoded.is_ok(), case.accepted, "{}: {decoded:?}", case.name);
                covered.insert(case.frame.as_str());
            }
            // The relay only encodes these frames; positive vectors pin its bytes.
            RELAY_TO_CLIENT => {}
            other => panic!("{}: unknown direction {other}", case.name),
        }
    }
    assert_eq!(
        covered,
        BTreeSet::from(["WS2P", "WS2R", "WS2T", "WS2U", "WS2V", "WS2X"])
    );
}

#[test]
fn proofs_follow_shared_authentication_vectors() {
    let vector: ProofVector = session_case("relay-control-proof-authentication");
    let relay = relay_identity(&vector.relay_identity_b64);
    for case in vector.cases {
        let challenge = challenge(
            challenge_purpose(case.challenge_purpose),
            &case.challenge_id_b64,
            &case.challenge_nonce_b64,
            &case.challenge_expires_at,
        );
        let now = unix_seconds(&case.now_unix_seconds);
        let init = bytes(&case.init_b64);
        let proof = bytes(&case.proof_b64);
        let outcome = match case.operation.as_str() {
            "registration" => authenticate_registration(
                RegisterInit::decode(&init).expect("proof vectors carry valid frames"),
                challenge,
                &relay,
                &RegisterProof::decode(&proof).expect("proof vectors carry valid frames"),
                now,
            )
            .map(drop),
            "stop" => authenticate_stop(
                StopInit::decode(&init).expect("proof vectors carry valid frames"),
                challenge,
                &relay,
                &StopProof::decode(&proof).expect("proof vectors carry valid frames"),
                now,
            )
            .map(drop),
            other => panic!("{}: unknown operation {other}", case.name),
        };
        assert_eq!(
            outcome.is_ok(),
            case.authenticated,
            "{}: {outcome:?}",
            case.name
        );
    }
}
