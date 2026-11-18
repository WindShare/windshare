use super::*;

#[test]
fn forwarding_requires_an_explicit_trusted_peer_and_canonicalizes_source_identity() {
    assert_eq!(
        source_key(Some("192.0.2.2"), Some("203.0.113.9"), "192.0.2.1").unwrap(),
        "192.0.2.2"
    );
    assert_eq!(
        source_key(
            Some("::ffff:192.0.2.1"),
            Some("::ffff:203.0.113.9"),
            "192.0.2.1"
        )
        .unwrap(),
        "203.0.113.9"
    );
    assert_eq!(
        source_key(Some("2001:db8::1"), None, "").unwrap(),
        "2001:db8::1"
    );
    assert!(source_key(Some("192.0.2.1"), None, "192.0.2.1").is_err());
    assert!(source_key(Some("192.0.2.1"), Some("forged"), "192.0.2.1").is_err());
    assert!(source_key(Some("192.0.2.1"), None, "typo").is_err());
}
