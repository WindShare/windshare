use super::*;

#[test]
fn canonical_endpoint_identities_match_shared_vectors() {
    use base64::{Engine, engine::general_purpose::STANDARD};

    let vectors: serde_json::Value =
        serde_json::from_str(include_str!("../../../../core/testvectors/v2-session.json")).unwrap();
    let cases = vectors["cases"]
        .as_array()
        .unwrap()
        .iter()
        .find(|case| case["name"] == "v2-relay-endpoint-normalization")
        .unwrap();
    for case in cases["cases"].as_array().unwrap() {
        if case["accepted"] != true {
            continue;
        }
        let canonical = case["relayIdentityEndpoint"].as_str().unwrap();
        let endpoint = Endpoint::parse(canonical).unwrap_or_else(|error| {
            panic!("{}: {error}", case["name"]);
        });
        let expected = STANDARD
            .decode(case["relayIdentityB64"].as_str().unwrap())
            .unwrap();
        assert_eq!(
            endpoint.identity.as_bytes().as_slice(),
            expected,
            "{}",
            case["name"]
        );
        assert_eq!(endpoint.url, canonical);
    }
}

#[test]
fn endpoint_identity_uses_exact_public_bytes() {
    let endpoint = Endpoint::parse("wss://relay.example/base/v2/ws").unwrap();
    let digest: [u8; 32] = Sha256::new()
        .chain_update(RELAY_IDENTITY_DOMAIN)
        .chain_update(b"wss://relay.example/base/v2/ws")
        .finalize()
        .into();
    assert_eq!(endpoint.identity.as_bytes(), &digest);
    assert_eq!(endpoint.path, "/base/v2/ws");
    assert!(Endpoint::parse("ws://[::1]:8080/v2/ws").is_ok());
    for invalid in [
        "https://relay.example/v2/ws",
        "wss://Relay.example/v2/ws",
        "wss://relay.example:443/v2/ws",
        "wss://relay.example/base/../v2/ws",
        "wss://relay.example/v2/ws?secret=x",
        "wss://user@relay.example/v2/ws",
        "wss://relay.example/v2/ws#fragment",
        "wss://xn--test.example/v2/ws",
        "ws://127.1/v2/ws",
    ] {
        assert!(Endpoint::parse(invalid).is_err(), "{invalid}");
    }
}
