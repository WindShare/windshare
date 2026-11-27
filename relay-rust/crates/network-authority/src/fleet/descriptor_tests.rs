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

fn inspect_node(origin: &str, bytes: &[u8], now: u64) -> Result<Discovery, DescriptorError> {
    inspect(origin, bytes, now, "issuer", &["https://api.example"])
}

fn valid_payload(node_key: &str, node_id: &str) -> Value {
    json!({
        "version": 2,
        "authority": { "url": "https://api.example/", "issuer": "issuer" },
        "node_id": node_id,
        "node_key": node_key,
        "platform": "native",
        "relay_endpoint": "wss://relay.example/v2/ws",
        "services": { "relay": true, "stun": true },
        "public_relay":{"state":"enabled","anonymous":true},
        "stun": ["stun:relay.example:3478"],
        "zone": "global",
        "region": "tokyo",
        "public_capacity_bytes_per_second": 12_500_000,
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
    let discovery = inspect_node(ORIGIN, &raw, NOW).unwrap();
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
        inspect_node("http://relay.example", &sign_envelope(&key, &valid), NOW).unwrap_err();
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
        let error = inspect_node(ORIGIN, &raw, NOW).unwrap_err();
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

    let mut at_skew_limit = valid_payload(&node_key, &node_id);
    at_skew_limit["issued_at"] = json!(NOW + trust::CLOCK_SKEW);
    at_skew_limit["expires_at"] = json!(NOW + trust::CLOCK_SKEW + MAX_DESCRIPTOR_LIFETIME_SECONDS);
    inspect_node(ORIGIN, &sign_envelope(&key, &at_skew_limit), NOW)
        .expect("descriptor within shared trust::CLOCK_SKEW should be accepted");

    for mutate in [
        |p: &mut Value| p["expires_at"] = json!(NOW),
        |p: &mut Value| {
            p["issued_at"] = json!(NOW + trust::CLOCK_SKEW + 1);
            p["expires_at"] = json!(NOW + trust::CLOCK_SKEW + 1 + MAX_DESCRIPTOR_LIFETIME_SECONDS);
        },
        |p: &mut Value| p["expires_at"] = json!(NOW + 301),
        |p: &mut Value| {
            p["issued_at"] = json!(NOW + 20);
            p["expires_at"] = json!(NOW + 10);
        },
    ] {
        let mut payload = valid_payload(&node_key, &node_id);
        mutate(&mut payload);
        let error = inspect_node(ORIGIN, &sign_envelope(&key, &payload), NOW).unwrap_err();
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
        let error = inspect_node(ORIGIN, &sign_envelope(&key, &payload), NOW).unwrap_err();
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
        (DiscoveryFailure::AuthorityMismatch, 422),
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

#[test]
fn missing_or_partial_authority_settings_have_a_distinct_discovery_failure() {
    let (key, node_key, node_id) = node_identity(10);
    for authority in [
        Value::Null,
        json!({"issuer":"issuer"}),
        json!({"url":"https://api.example"}),
        json!({"url":"https://api.example","issuer":"different"}),
    ] {
        let mut payload = valid_payload(&node_key, &node_id);
        payload["authority"] = authority;
        assert_eq!(
            inspect_node(ORIGIN, &sign_envelope(&key, &payload), NOW)
                .unwrap_err()
                .failure,
            DiscoveryFailure::AuthorityMismatch
        );
    }
}

#[test]
fn authority_url_matches_by_canonical_origin_and_rejects_non_origin_forms() {
    let (key, node_key, node_id) = node_identity(11);
    for url in [
        "https://api.example",
        "https://api.example/",
        "https://API.example",
        "HTTPS://API.example/",
        "https://api.example:443",
        "https://API.example:443/",
    ] {
        let mut payload = valid_payload(&node_key, &node_id);
        payload["authority"] = json!({"url": url, "issuer": "issuer"});
        let raw = sign_envelope(&key, &payload);
        assert!(
            inspect(ORIGIN, &raw, NOW, "issuer", &["https://api.example/"]).is_ok(),
            "expected equivalent origin '{url}' to match"
        );
    }

    for rejected in [
        "http://api.example",
        "https:api.example",
        "https://api.example:8443",
        "https://api.example:0",
        "https://api.example/v1",
        "https://api.example//",
        "https://api.example/.",
        "https://api.example?foo=1",
        "https://api.example?",
        "https://api.example#frag",
        "https://api.example#",
        "https://user@api.example",
        "https://@api.example",
        " https://api.example ",
        "https://api.example\n",
    ] {
        let mut payload = valid_payload(&node_key, &node_id);
        payload["authority"] = json!({"url": rejected, "issuer": "issuer"});
        assert_eq!(
            inspect_node(ORIGIN, &sign_envelope(&key, &payload), NOW)
                .unwrap_err()
                .failure,
            DiscoveryFailure::AuthorityMismatch,
            "expected '{rejected}' to be rejected"
        );
    }
}
