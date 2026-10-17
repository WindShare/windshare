use super::*;

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
