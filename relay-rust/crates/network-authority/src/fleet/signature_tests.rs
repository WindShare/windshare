use super::{Envelope, Root};
use serde::Deserialize;

#[derive(Deserialize)]
struct Vectors {
    network: String,
    roots: Vec<Root>,
    entries: Vec<Case>,
}
#[derive(Deserialize)]
struct Case {
    name: String,
    valid: bool,
    envelope: Envelope,
}

#[test]
fn signed_directory_agrees_with_native_and_browser() {
    let vectors: Vectors = serde_json::from_str(include_str!(
        "../../../../../testdata/network/directory.json"
    ))
    .unwrap();
    for case in vectors.entries {
        let result = case
            .envelope
            .verify(&vectors.network, &vectors.roots, false);
        assert_eq!(
            result.is_ok(),
            case.valid,
            "{}: {:?}",
            case.name,
            result.err()
        );
    }
}

#[test]
fn rejects_oversized_publication_and_raw_wire_padding() {
    use super::{MAX_BYTES, SigningKey};
    let vectors: Vectors = serde_json::from_str(include_str!(
        "../../../../../testdata/network/directory.json"
    ))
    .unwrap();
    let envelope = &vectors.entries[0].envelope;
    let mut wire = serde_json::to_vec(envelope).unwrap();
    wire.resize(MAX_BYTES, b' ');
    assert!(Envelope::decode(&wire).is_ok());
    wire.push(b' ');
    assert!(Envelope::decode(&wire).is_err());

    let mut payload = envelope
        .verify(&vectors.network, &vectors.roots, false)
        .unwrap();
    let mut node = payload.nodes.remove(0);
    node.spec.region = "\u{1}".repeat(128);
    node.spec.provider.clone_from(&node.spec.region);
    node.spec.operator.clone_from(&node.spec.region);
    for i in 0..256 {
        node.spec.alias = format!("n{i}");
        node.spec.relay = format!("https://n{i}.example");
        payload.nodes.push(node.clone());
    }
    let grant = envelope
        .certificate
        .verify(&vectors.network, &vectors.roots)
        .unwrap();
    assert!(
        payload
            .advance(
                envelope.certificate.clone(),
                &grant,
                &SigningKey::from_bytes(&[2; 32]),
                &vectors.roots,
                payload.issued_at
            )
            .unwrap_err()
            .to_string()
            .contains("too large")
    );
}

#[test]
fn relay_spelling_and_text_match_the_shared_edge_table() {
    use super::super::{endpoints::relay_identity, topology::valid_text};
    #[derive(Deserialize)]
    struct Edges {
        invalid_edge_code_points: Vec<u32>,
    }
    let edges: Edges = serde_json::from_str(include_str!(
        "../../../../../testdata/network/text-edges.json"
    ))
    .unwrap();
    for point in edges.invalid_edge_code_points {
        let c = char::from_u32(point).unwrap();
        assert!(!valid_text(&format!("{c}region")));
        assert!(!valid_text(&format!("region{c}")));
        assert!(relay_identity(&format!("https://relay.example/base{c}")).is_err());
    }
    assert_eq!(
        relay_identity("https://relay.example/a\u{0085}b").unwrap(),
        "wss://relay.example/a%C2%85b/v2/ws"
    );
    assert_eq!(
        relay_identity("https://[::ffff:c000:201]:443/").unwrap(),
        "wss://192.0.2.1/v2/ws"
    );
}
#[test]
fn directory_relay_identities_match_the_wire_contract() {
    use super::super::endpoints::relay_identity;
    let vectors: serde_json::Value = serde_json::from_str(include_str!(
        "../../../../../core/testvectors/v2-session.json"
    ))
    .unwrap();
    let group = vectors["cases"]
        .as_array()
        .unwrap()
        .iter()
        .find(|c| c["name"] == "v2-relay-endpoint-normalization")
        .unwrap();
    for case in group["cases"].as_array().unwrap() {
        let raw = case["relayBaseUrl"].as_str().unwrap();
        if raw.starts_with("https://") && !raw.contains('?') {
            let actual = relay_identity(raw);
            assert_eq!(
                actual.ok().as_deref(),
                case["relayIdentityEndpoint"].as_str(),
                "{raw}"
            );
        }
    }
}
