extern crate std;

use serde::Deserialize;
use std::{string::String, vec, vec::Vec};

use super::*;
use crate::test_vectors::{array, bytes, session_case};

const PREFIX_BYTES: usize = 8;
const SESSION_BYTES: usize = 8;
const LENGTH_OFFSET: usize = PREFIX_BYTES + SESSION_BYTES;
const OPAQUE_HEADER_BYTES: usize = LENGTH_OFFSET + 4;

#[derive(Deserialize)]
struct Vector {
    #[serde(rename = "opaqueRouteB64")]
    opaque_route: String,
    #[serde(rename = "opaqueRelaySessionIdB64")]
    session_id: String,
    #[serde(rename = "sessionAdmittedB64")]
    admitted: String,
    #[serde(rename = "descriptorUploadB64")]
    descriptor_upload: String,
    #[serde(rename = "descriptorDeliveryB64")]
    descriptor_delivery: String,
}

#[test]
fn session_controls_and_opaque_metadata_follow_shared_vectors() {
    let vector: Vector = session_case("fresh-relay-registration-proof");
    let encoded = bytes(&vector.opaque_route);
    let header = OpaqueHeader::decode(&encoded).unwrap();
    let session = RelaySessionId::new(array(&vector.session_id)).unwrap();
    assert_eq!(header.session(), session);
    assert_eq!(header.encoded_bytes(), encoded.len());
    assert_eq!(
        SessionAdmitted::decode(&bytes(&vector.admitted))
            .unwrap()
            .session(),
        session
    );
    assert_eq!(RelaySessionId::new([0; SESSION_BYTES]), None);
    let upload = bytes(&vector.descriptor_upload);
    let object = crate::descriptor_upload::DescriptorUpload::decode(&upload).unwrap();
    assert_eq!(
        encode_descriptor(session, object.object()).unwrap(),
        bytes(&vector.descriptor_delivery)
    );
}

#[test]
fn descriptor_delivery_keeps_the_full_boundary_and_rejects_invalid_sizes() {
    let session = RelaySessionId::new([1; SESSION_BYTES]).unwrap();
    assert!(encode_descriptor(session, &[]).is_err());
    assert!(encode_descriptor(session, &vec![0; MAX_DESCRIPTOR_BYTES + 1]).is_err());
    let object = vec![7; MAX_DESCRIPTOR_BYTES];
    let encoded = encode_descriptor(session, &object).unwrap();
    assert_eq!(
        encoded.len(),
        DESCRIPTOR_HEADER_BYTES + MAX_DESCRIPTOR_BYTES
    );
    assert_eq!(&encoded[DESCRIPTOR_HEADER_BYTES..], object);
}

fn invalid_layouts(encoded: &[u8]) -> Vec<Vec<u8>> {
    let mut invalid: Vec<Vec<u8>> = (0..encoded.len())
        .map(|end| encoded[..end].to_vec())
        .collect();
    let mut extended = encoded.to_vec();
    extended.push(0);
    invalid.push(extended);
    for index in 0..PREFIX_BYTES {
        let mut bad = encoded.to_vec();
        bad[index] ^= 1;
        invalid.push(bad);
    }
    let mut zero = encoded.to_vec();
    zero[PREFIX_BYTES..LENGTH_OFFSET].fill(0);
    invalid.push(zero);
    invalid
}

#[test]
fn exact_layout_checks_precede_session_authority() {
    let vector: Vector = session_case("fresh-relay-registration-proof");
    for encoded in invalid_layouts(&bytes(&vector.opaque_route)) {
        assert!(OpaqueHeader::decode(&encoded).is_err());
    }
    for encoded in invalid_layouts(&bytes(&vector.admitted)) {
        assert!(SessionAdmitted::decode(&encoded).is_err());
    }
}

#[test]
fn opaque_bounds_match_wire_contract_without_inspecting_ciphertext() {
    let vector: Vector = session_case("fresh-relay-registration-proof");
    let original = bytes(&vector.opaque_route);
    for length in [
        0,
        1,
        MAX_OPAQUE_CIPHERTEXT_BYTES,
        MAX_OPAQUE_CIPHERTEXT_BYTES + 1,
    ] {
        let mut frame = original[..OPAQUE_HEADER_BYTES].to_vec();
        frame[LENGTH_OFFSET..].copy_from_slice(&length.to_be_bytes());
        frame.extend(vec![0xff; length as usize]);
        assert_eq!(
            OpaqueHeader::decode(&frame).is_ok(),
            (1..=MAX_OPAQUE_CIPHERTEXT_BYTES).contains(&length),
            "payload length {length}"
        );
    }
    let mut oversized = original;
    oversized[LENGTH_OFFSET..OPAQUE_HEADER_BYTES].copy_from_slice(&u32::MAX.to_be_bytes());
    assert_eq!(OpaqueHeader::decode(&oversized), Err(FrameError::Field));
}
