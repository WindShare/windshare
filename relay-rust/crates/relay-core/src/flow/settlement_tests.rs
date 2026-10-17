extern crate std;

use super::*;
use crate::test_vectors::{Decimal, array, bytes, session_case};
use serde::Deserialize;
use std::{fmt::Debug, string::String, vec::Vec};

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

fn check_vectors<T: Copy + Debug + PartialEq>(
    family: &str,
    new: fn(RelaySessionId, Counts) -> Result<T, FrameError>,
    decode: fn(&[u8]) -> Result<T, FrameError>,
    encode: fn(T) -> [u8; FRAME_BYTES],
) {
    let vector: Vector = session_case(family);
    let session = RelaySessionId::new(array(&vector.relay_session_id_b64)).unwrap();
    for case in vector.counts {
        let total = Counts::new(
            case.total_frames.parse().unwrap(),
            case.total_encoded_bytes.parse().unwrap(),
        );
        let encoded = bytes(&case.encoded_b64);
        let constructed = new(session, total);
        let decoded = decode(&encoded);
        if case.accepted {
            let control = constructed.unwrap();
            assert_eq!(encode(control).as_slice(), encoded, "{}", case.name);
            assert_eq!(decoded, Ok(control), "{}", case.name);
        } else {
            assert_eq!(constructed, Err(FrameError::Field), "{}", case.name);
            assert_eq!(decoded, Err(FrameError::Field), "{}", case.name);
        }
    }
    for case in vector.malformed {
        assert!(decode(&bytes(&case.encoded_b64)).is_err(), "{}", case.name);
    }
}

#[test]
fn settlement_notice_follows_shared_vectors() {
    check_vectors(
        "relay-settlement-notice",
        SettlementNotice::new,
        SettlementNotice::decode,
        SettlementNotice::encode,
    );
}

#[test]
fn settlement_ack_follows_shared_vectors() {
    check_vectors(
        "relay-settlement-ack",
        SettlementAck::new,
        SettlementAck::decode,
        SettlementAck::encode,
    );
}
