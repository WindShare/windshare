use super::*;
use crate::node::Identity;
use serde_json::json;

pub(in crate::discovery) fn padded_gossip_envelope(
    label: usize,
    now: u64,
    target_wire_len: usize,
) -> Envelope {
    let descriptor_b64_len = target_wire_len
        .checked_sub(ENVELOPE_JSON_FRAMING + SIGNATURE_B64_LEN)
        .expect("target wire length below minimum envelope framing");
    let raw_payload_len = (descriptor_b64_len / 4) * 3
        + match descriptor_b64_len % 4 {
            0 => 0,
            2 => 1,
            3 => 2,
            _ => panic!("impossible unpadded base64url length {descriptor_b64_len}"),
        };

    let identity = Identity::generate().unwrap();
    let signing_key = identity.key();
    let key_b64 = B64.encode(signing_key.verifying_key().to_bytes());
    let mut payload = json!({
        "version": DESCRIPTOR_VERSION,
        "node_id": identity.id(),
        "node_key": key_b64,
        "relay_endpoint": format!("wss://padded{label}.example/v2/ws"),
        "services": { "relay": true, "stun": false },
        "stun": [],
        "zone": "global",
        "region": "",
        "operator": "",
        "provider": "",
        "platform": NODE_PLATFORM_NATIVE,
        "classes": ["owner", "standard", "lowest"],
        "issued_at": now,
        "expires_at": now + Purpose::Gossip.lifetime(),
        "public_relay": PublicRelay::Enabled { anonymous: true },
        "authority": null,
    });
    let base_len = serde_json::to_vec(&payload).unwrap().len();
    payload["operator"] = json!("a".repeat(raw_payload_len - base_len));
    let raw_payload = serde_json::to_vec(&payload).unwrap();
    assert_eq!(raw_payload.len(), raw_payload_len);

    let signed = trust::sign(Purpose::Gossip.domain(), &raw_payload, &signing_key);
    let envelope = Envelope {
        version: DESCRIPTOR_VERSION,
        descriptor: B64.encode(&raw_payload),
        signature: B64.encode(&signed[raw_payload.len()..]),
    };
    let wire = serde_json::to_vec(&envelope).unwrap();
    assert_eq!(wire.len(), target_wire_len);
    assert_eq!(envelope.encoded_len(), wire.len());
    envelope
}

#[test]
fn envelope_verify_enforces_serialized_wire_record_limit() {
    let now = crate::node::now().unsigned_abs();

    let exact_limit = padded_gossip_envelope(1, now, MAX_RECORD);
    let record = exact_limit.verify(Purpose::Gossip, now).unwrap();
    assert_eq!(record.endpoint, "wss://padded1.example/v2/ws");

    let one_byte_over = padded_gossip_envelope(2, now, MAX_RECORD + 1);
    assert!(one_byte_over.verify(Purpose::Gossip, now).is_err());
}

#[test]
fn json_array_bound_covers_empty_and_maximal_record_arrays() {
    assert_eq!(json_array_bound(0), 2);
    let sample_max = 8 * MAX_RECORD + 7 + 2;
    assert!(sample_max <= json_array_bound(8));
    let peers_max = 128 * MAX_RECORD + 127 + 2;
    assert!(peers_max <= json_array_bound(128));
}

fn signed_descriptor(label: usize, purpose: Purpose, issued_at: u64, expires_at: u64) -> Envelope {
    let identity = Identity::generate().unwrap();
    let signing_key = identity.key();
    let key_b64 = B64.encode(signing_key.verifying_key().to_bytes());
    let payload = json!({
        "version": DESCRIPTOR_VERSION,
        "node_id": identity.id(),
        "node_key": key_b64,
        "relay_endpoint": format!("wss://skew{label}.example/v2/ws"),
        "services": { "relay": true, "stun": false },
        "stun": [],
        "zone": "global",
        "region": "",
        "operator": "",
        "provider": "",
        "platform": NODE_PLATFORM_NATIVE,
        "classes": ["owner", "standard", "lowest"],
        "issued_at": issued_at,
        "expires_at": expires_at,
        "public_relay": PublicRelay::Enabled { anonymous: true },
        "authority": null,
    });
    let raw_payload = serde_json::to_vec(&payload).unwrap();
    let signed = trust::sign(purpose.domain(), &raw_payload, &signing_key);
    Envelope {
        version: DESCRIPTOR_VERSION,
        descriptor: B64.encode(&raw_payload),
        signature: B64.encode(&signed[raw_payload.len()..]),
    }
}

#[test]
fn envelope_verify_matches_shared_clock_skew_for_live_and_gossip() {
    let now = crate::node::now().unsigned_abs();
    for (index, purpose) in [Purpose::Live, Purpose::Gossip].into_iter().enumerate() {
        let lifetime = purpose.lifetime();
        let at_limit = now + trust::CLOCK_SKEW;
        assert!(
            signed_descriptor(index, purpose, at_limit, at_limit + lifetime)
                .verify(purpose, now)
                .is_ok(),
            "descriptor at shared trust::CLOCK_SKEW boundary should be accepted"
        );

        let beyond_limit = now + trust::CLOCK_SKEW + 1;
        assert!(
            signed_descriptor(index + 10, purpose, beyond_limit, beyond_limit + lifetime)
                .verify(purpose, now)
                .is_err(),
            "descriptor beyond shared trust::CLOCK_SKEW should be rejected"
        );

        assert!(
            signed_descriptor(index + 20, purpose, now - lifetime, now)
                .verify(purpose, now)
                .is_err(),
            "expired descriptor must remain rejected without skew grace"
        );
    }
}
