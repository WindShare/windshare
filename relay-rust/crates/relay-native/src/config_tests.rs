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
fn numeric_hosts_cannot_be_used_as_dns_identities() {
    // The shared rejected vectors contain base URLs, while Endpoint consumes
    // canonical WebSocket endpoints. Exercise host rejection at its own boundary.
    for host in [
        "0x7f.0.0.1",
        "0x7f000001",
        "relay.123",
        "relay.0x10",
        "relay.0X1f",
        "relay.0x",
        "relay.123.",
        "0x",
        "0X",
        "0xcafe",
        "relay.09",
    ] {
        let endpoint = format!("wss://{host}:8443/v2/ws");
        assert!(Endpoint::parse(&endpoint).is_err(), "{endpoint}");
    }
    for host in [
        "relay123",
        "123.example",
        "relay.0x1g",
        "relay.x10",
        "0xg",
        "relay.1e3",
        "relay.0x0x1",
        "192.0.2.1",
        "[::1]",
    ] {
        let endpoint = format!("wss://{host}:8443/v2/ws");
        assert!(Endpoint::parse(&endpoint).is_ok(), "{endpoint}");
    }
}

#[test]
fn endpoint_hosts_reject_unspecified_addresses_but_allow_local_networks() {
    for host in ["0.0.0.0", "[::]", "[::ffff:0:0]"] {
        let endpoint = format!("ws://{host}:8484/v2/ws");
        let error = Endpoint::parse(&endpoint).expect_err("unspecified endpoint host");
        assert!(
            error.to_string().contains("unspecified"),
            "{endpoint}: {error}"
        );
    }
    for host in [
        "127.0.0.1",
        "[::1]",
        "192.168.1.10",
        "[fd00::1]",
        "relay.example",
    ] {
        let endpoint = format!("ws://{host}:8484/v2/ws");
        assert!(Endpoint::parse(&endpoint).is_ok(), "{endpoint}");
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
