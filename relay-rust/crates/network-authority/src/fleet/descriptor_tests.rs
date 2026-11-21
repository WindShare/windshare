use super::*;
use crate::Fault;
use ed25519_dalek::{Signer, SigningKey};
use serde_json::{Value, json};

const NOW: u64 = 1_700_000_000;
const ORIGIN: &str = "https://relay.example";

fn node_identity(seed: u8) -> (SigningKey, String, String) {
    let key = SigningKey::from_bytes(&[seed; 32]);
    let raw_pub = key.verifying_key().to_bytes();
    let mut digest = Sha256::new();
    digest.update(b"windshare/node-identity/v1\0");
    digest.update(raw_pub);
    (key, B64.encode(raw_pub), B64.encode(digest.finalize()))
}

fn valid_payload(node_key: &str, node_id: &str) -> Value {
    json!({
        "version": 2,
        "node_id": node_id,
        "node_key": node_key,
        "relay_endpoint": "wss://relay.example/v2/ws",
        "services": { "relay": true, "stun": true },
        "stun": ["stun:relay.example:3478"],
        "zone": "global",
        "region": "tokyo",
        "capacity_bytes_per_second": 12_500_000,
        "issued_at": NOW,
        "expires_at": NOW + 300
    })
}

fn sign_envelope(key: &SigningKey, payload: &Value) -> Vec<u8> {
    let bytes = serde_json::to_vec(payload).unwrap();
    let signature = key.sign(&[b"windshare/node-descriptor/v2\0".as_slice(), &bytes].concat());
    serde_json::to_vec(&json!({
        "version": 2,
        "descriptor": B64.encode(bytes),
        "signature": B64.encode(signature.to_bytes()),
    }))
    .unwrap()
}

#[test]
fn inspect_accepts_valid_signed_descriptor() {
    let (key, node_key, node_id) = node_identity(7);
    let raw = sign_envelope(&key, &valid_payload(&node_key, &node_id));
    let discovery = inspect(ORIGIN, &raw, NOW).unwrap();
    assert_eq!(discovery.node_key, node_key);
    assert_eq!(discovery.relay, ORIGIN);
    assert_eq!(discovery.roles, vec![Role::Relay, Role::Stun]);
    assert_eq!(discovery.stun, vec!["stun:relay.example:3478"]);
    assert_eq!(discovery.capacity_mbps, Some(100));
}

#[test]
fn inspect_classifies_malformed_oversized_and_unsigned_descriptors() {
    let (key, node_key, node_id) = node_identity(8);
    let valid = valid_payload(&node_key, &node_id);

    let bad_origin =
        inspect("http://relay.example", &sign_envelope(&key, &valid), NOW).unwrap_err();
    assert_eq!(bad_origin.failure, DiscoveryFailure::InvalidUrl);

    let oversized = vec![b' '; MAX_DESCRIPTOR + 1];
    let bad_version = serde_json::to_vec(&json!({
        "version": 1,
        "descriptor": "",
        "signature": ""
    }))
    .unwrap();
    let bad_base64 = serde_json::to_vec(&json!({
        "version": 2,
        "descriptor": "!!!",
        "signature": ""
    }))
    .unwrap();
    let bad_payload_json = serde_json::to_vec(&json!({
        "version": 2,
        "descriptor": B64.encode(b"{not-json"),
        "signature": ""
    }))
    .unwrap();
    let mut bad_sig: Value = serde_json::from_slice(&sign_envelope(&key, &valid)).unwrap();
    bad_sig["signature"] = json!(B64.encode([0u8; 64]));
    let mut wrong_payload_version = valid;
    wrong_payload_version["version"] = json!(1);

    for raw in [
        oversized,
        b"{not-json".to_vec(),
        bad_version,
        bad_base64,
        bad_payload_json,
        serde_json::to_vec(&bad_sig).unwrap(),
        sign_envelope(&key, &wrong_payload_version),
    ] {
        let error = inspect(ORIGIN, &raw, NOW).unwrap_err();
        assert_eq!(
            error.failure,
            DiscoveryFailure::InvalidDescriptor,
            "{error}"
        );
        assert_ne!(error.to_string(), "");
    }
}

#[test]
fn inspect_classifies_expired_skewed_and_mismatched_descriptors() {
    let (key, node_key, node_id) = node_identity(9);

    for mutate in [
        |p: &mut Value| p["expires_at"] = json!(NOW),
        |p: &mut Value| p["issued_at"] = json!(NOW + 61),
        |p: &mut Value| p["expires_at"] = json!(NOW + 301),
        |p: &mut Value| {
            p["issued_at"] = json!(NOW + 20);
            p["expires_at"] = json!(NOW + 10);
        },
    ] {
        let mut payload = valid_payload(&node_key, &node_id);
        mutate(&mut payload);
        let error = inspect(ORIGIN, &sign_envelope(&key, &payload), NOW).unwrap_err();
        assert_eq!(
            error.failure,
            DiscoveryFailure::DescriptorExpired,
            "{error}"
        );
    }

    for mutate in [
        |p: &mut Value| p["node_id"] = json!("wrong-id"),
        |p: &mut Value| p["relay_endpoint"] = json!("wss://other.example/v2/ws"),
        |p: &mut Value| p["services"] = json!({"relay": false, "stun": false}),
        |p: &mut Value| p["stun"] = json!([]),
        |p: &mut Value| p["stun"] = json!(["stun:other.example:3478"]),
        |p: &mut Value| p["stun"] = json!(["not-a-stun-url"]),
        |p: &mut Value| p["stun"] = json!(["stun:relay.example:3478", "stun:relay.example:3478"]),
    ] {
        let mut payload = valid_payload(&node_key, &node_id);
        mutate(&mut payload);
        let error = inspect(ORIGIN, &sign_envelope(&key, &payload), NOW).unwrap_err();
        assert_eq!(
            error.failure,
            DiscoveryFailure::DescriptorMismatch,
            "{error}"
        );
    }
}

#[test]
fn discovery_failure_wire_reasons_and_statuses_stay_in_sync() {
    for (failure, expected_status) in [
        (DiscoveryFailure::InvalidUrl, 400),
        (DiscoveryFailure::ReservedHost, 400),
        (DiscoveryFailure::DnsFailed, 502),
        (DiscoveryFailure::NonPublicAddress, 422),
        (DiscoveryFailure::Unreachable, 502),
        (DiscoveryFailure::RedirectRefused, 502),
        (DiscoveryFailure::HttpStatus { status: 404 }, 502),
        (DiscoveryFailure::InvalidDescriptor, 422),
        (DiscoveryFailure::DescriptorExpired, 422),
        (DiscoveryFailure::DescriptorMismatch, 422),
    ] {
        assert_eq!(failure.http_status(), expected_status);
        let encoded = serde_json::to_value(Fault::node_discovery(failure)).unwrap();
        assert_eq!(encoded["kind"], "node_discovery");
        assert_eq!(encoded["reason"], failure.reason());
        if let DiscoveryFailure::HttpStatus { status } = failure {
            assert_eq!(encoded["status"], status);
        } else {
            assert!(encoded.get("status").is_none());
        }
    }
}
