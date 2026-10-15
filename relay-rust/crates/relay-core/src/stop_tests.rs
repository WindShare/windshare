extern crate std;

use std::string::String;

use serde::Deserialize;

use super::{StopInit, StopProof, Stopped, authenticate_stop};
use crate::{
    challenge::ChallengePurpose,
    test_vectors::{
        Decimal, array, bytes, challenge, last_live_instant, relay_identity, session_case,
    },
};

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct StopVector {
    relay_identity_b64: String,
    stop_id_b64: String,
    stop_challenge_id_b64: String,
    stop_challenge_nonce_b64: String,
    stop_expires_at: Decimal,
    stop_init_b64: String,
    stop_challenge_b64: String,
    stop_preimage_b64: String,
    stop_proof_b64: String,
    stopped_b64: String,
}

#[test]
fn stop_reproduces_shared_bytes_and_authority() {
    let vector: StopVector = session_case("fresh-relay-registration-proof");
    let relay = relay_identity(&vector.relay_identity_b64);
    let init = StopInit::decode(&bytes(&vector.stop_init_b64)).expect("shared WS2X");
    assert_eq!(init.relay(), relay);
    assert_eq!(init.stop_id().as_bytes(), &array(&vector.stop_id_b64));
    assert_eq!(
        Stopped::new(init.stop_id()).encode().as_slice(),
        bytes(&vector.stopped_b64)
    );

    let challenge = challenge(
        ChallengePurpose::Stop,
        &vector.stop_challenge_id_b64,
        &vector.stop_challenge_nonce_b64,
        &vector.stop_expires_at,
    );
    assert_eq!(
        challenge.encode().as_slice(),
        bytes(&vector.stop_challenge_b64)
    );
    assert_eq!(
        init.preimage(&challenge).written(),
        bytes(&vector.stop_preimage_b64)
    );

    let proof = StopProof::decode(&bytes(&vector.stop_proof_b64)).expect("shared WS2V");
    let now = last_live_instant(&challenge);
    let authority =
        authenticate_stop(init, challenge, &relay, &proof, now).expect("shared STOP proof");
    assert_eq!(authority.init(), &init);
}
