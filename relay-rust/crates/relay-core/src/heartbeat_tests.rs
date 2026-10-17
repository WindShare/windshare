use super::*;
use crate::test_vectors::{bytes, session_case};

#[test]
fn probes_and_acknowledgements_match_shared_client_vectors() {
    for name in [
        "fresh-relay-registration-proof",
        "relay-capacity-registration",
    ] {
        let case: serde_json::Value = session_case(name);
        let nonce = case["connectionProbeNonce"]
            .as_str()
            .unwrap()
            .parse::<u64>()
            .unwrap();
        let probe_bytes = bytes(case["connectionProbeB64"].as_str().unwrap());
        let ack_bytes = bytes(case["connectionProbeAckB64"].as_str().unwrap());
        let probe = ConnectionProbe::decode(&probe_bytes).unwrap();
        let ack = ConnectionProbeAck::decode(&ack_bytes).unwrap();
        assert_eq!(probe.nonce(), nonce);
        assert_eq!(ack.nonce(), nonce);
        assert_eq!(probe.encode().as_slice(), probe_bytes);
        assert_eq!(ack.encode().as_slice(), ack_bytes);
        assert_eq!(probe.acknowledgement(), ack);
    }
}

#[test]
fn heartbeat_decoders_require_exact_headers_lengths_and_nonzero_nonces() {
    let probe = ConnectionProbe::new(NonZeroU64::new(1).unwrap());
    for acknowledgement in [false, true] {
        let valid = if acknowledgement {
            probe.acknowledgement().encode()
        } else {
            probe.encode()
        };
        let accepted = |bytes: &[u8]| {
            if acknowledgement {
                ConnectionProbeAck::decode(bytes).is_ok()
            } else {
                ConnectionProbe::decode(bytes).is_ok()
            }
        };
        for end in 0..valid.len() {
            assert!(!accepted(&valid[..end]));
        }
        let mut trailing = valid.to_vec();
        trailing.push(0);
        assert!(!accepted(&trailing));
        for offset in 0..wire::HEADER_BYTES {
            let mut invalid = valid;
            invalid[offset] ^= 1;
            assert!(!accepted(&invalid));
        }
        let mut zero = valid;
        zero[wire::HEADER_BYTES..].fill(0);
        assert!(!accepted(&zero));
    }
    assert!(ConnectionProbe::decode(&probe.acknowledgement().encode()).is_err());
    assert!(ConnectionProbeAck::decode(&probe.encode()).is_err());
    let maximum = ConnectionProbe::new(NonZeroU64::MAX);
    assert_eq!(ConnectionProbe::decode(&maximum.encode()).unwrap(), maximum);
    assert_eq!(
        ConnectionProbeAck::decode(&maximum.acknowledgement().encode())
            .unwrap()
            .nonce(),
        u64::MAX
    );
}

#[test]
fn checkpoint_keeps_nonce_evidence_and_absolute_deadline() {
    let mut live = Liveness::established(UnixMillis(10));
    let probe = ConnectionProbe::new(NonZeroU64::new(7).unwrap());
    assert!(live.accept_probe(probe));
    assert!(!live.accept_probe(probe));
    assert!(live.accepted_inbound(UnixMillis(20)));
    let encoded = live.encode();
    let restored = Liveness::decode(&encoded).unwrap();
    assert_eq!(restored, live);
    assert_eq!(restored.last_probe_nonce(), 7);
    assert_eq!(restored.last_inbound(), UnixMillis(20));
    assert_eq!(
        restored.deadline(),
        UnixMillis(20).saturating_add(CONNECTION_SILENCE_TIMEOUT)
    );
    assert!(!live.accepted_inbound(UnixMillis(19)));
    assert_eq!(live, restored);
    for offset in [size_of::<u64>(), 2 * size_of::<u64>()] {
        let mut invalid = encoded;
        invalid[offset] ^= 1;
        assert!(Liveness::decode(&invalid).is_err());
    }
    assert!(Liveness::decode(&encoded[..encoded.len() - 1]).is_err());
    let mut trailing = encoded.to_vec();
    trailing.push(0);
    assert!(Liveness::decode(&trailing).is_err());
    let saturated = Liveness::established(UnixMillis(u64::MAX - 1));
    assert_eq!(saturated.deadline(), UnixMillis(u64::MAX));
    assert_eq!(Liveness::decode(&saturated.encode()).unwrap(), saturated);
}
