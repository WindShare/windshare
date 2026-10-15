extern crate std;

use std::{string::String, vec::Vec};

use serde::Deserialize;

use super::{ErrorFrame, RetryAfter};
use crate::test_vectors::{bytes, session_case};

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct PermanentRefusalVector {
    stopped_error_b64: String,
    resume_stale_error_b64: String,
}

#[derive(Deserialize)]
struct DecodingVector {
    cases: Vec<DecodingCase>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct DecodingCase {
    name: String,
    encoded_b64: String,
    accepted: bool,
}

fn accepted_decoding_case(name: &str) -> Vec<u8> {
    let vector: DecodingVector = session_case("relay-control-frame-decoding");
    let case = vector
        .cases
        .into_iter()
        .find(|case| case.name == name)
        .unwrap_or_else(|| panic!("decoding vectors must contain {name}"));
    assert!(case.accepted, "{name} must be a frame clients accept");
    bytes(&case.encoded_b64)
}

#[test]
fn permanent_refusals_match_shared_bytes() {
    let vector: PermanentRefusalVector = session_case("fresh-relay-registration-proof");
    assert_eq!(
        ErrorFrame::Stopped.encode().as_slice(),
        bytes(&vector.stopped_error_b64)
    );
    assert_eq!(
        ErrorFrame::ResumeStale.encode().as_slice(),
        bytes(&vector.resume_stale_error_b64)
    );
}

#[test]
fn retry_hints_stop_at_the_limit_every_client_accepts() {
    assert_eq!(
        RetryAfter::from_millis(RetryAfter::MAX_MILLISECONDS + 1),
        None
    );
    let limit = RetryAfter::from_millis(RetryAfter::MAX_MILLISECONDS).expect("limit is inclusive");
    assert_eq!(
        ErrorFrame::Admission(limit).encode().as_slice(),
        accepted_decoding_case("error/retry-at-limit")
    );
}
