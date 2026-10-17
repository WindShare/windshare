extern crate std;

use serde::Deserialize;
use std::{string::String, vec::Vec};

use super::*;
use crate::test_vectors::{Decimal, array, bytes, session_case};

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct Vector {
    relay_session_id_b64: String,
    counts: Vec<CountCase>,
    malformed: Vec<MalformedCase>,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct CountCase {
    name: String,
    total_frames: Decimal,
    total_encoded_bytes: Decimal,
    encoded_b64: String,
    accepted: bool,
}

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct MalformedCase {
    name: String,
    encoded_b64: String,
}

#[test]
fn cumulative_counts_follow_shared_vectors() {
    let vector: Vector = session_case("relay-delivery-consumed");
    let session = RelaySessionId::new(array(&vector.relay_session_id_b64)).unwrap();
    for case in vector.counts {
        let total = Counts::new(
            case.total_frames.parse().unwrap(),
            case.total_encoded_bytes.parse().unwrap(),
        );
        let encoded = bytes(&case.encoded_b64);
        let constructed = DeliveryConsumed::new(session, total);
        let decoded = DeliveryConsumed::decode(&encoded);
        if case.accepted {
            let feedback = constructed.unwrap();
            assert_eq!(feedback.encode().as_slice(), encoded, "{}", case.name);
            assert_eq!(decoded, Ok(feedback), "{}", case.name);
            assert_eq!(feedback.session(), session);
            assert_eq!(feedback.total(), total);
        } else {
            assert_eq!(constructed, Err(FrameError::Field), "{}", case.name);
            assert_eq!(decoded, Err(FrameError::Field), "{}", case.name);
        }
    }
}

#[test]
fn malformed_feedback_follows_shared_vectors() {
    let vector: Vector = session_case("relay-delivery-consumed");
    for case in vector.malformed {
        assert!(
            DeliveryConsumed::decode(&bytes(&case.encoded_b64)).is_err(),
            "{}",
            case.name
        );
    }
}
