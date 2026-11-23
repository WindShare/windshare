use super::*;
use serde_json::json;

#[test]
fn selects_one_credential_across_split_server_entries() {
    let servers = json!([
        {"urls":"stun:discovery.example"},
        {"urls":"stun:discovery.example", "username":"unused", "credential":"ignored"},
        {"urls":"turn:ignored.example", "username":"oversized", "credential":"x".repeat(MAX_CREDENTIAL_BYTES + 1)},
        {"urls":"turn:relay.example:3478?transport=udp","username":"alice","credential":"first"},
        {"urls":"turn:other.example","username":"bob","credential":"first"},
        {"urls":"turn:other.example","username":"alice","credential":"second"},
        {"urls":["turn:relay.example:3478?transport=udp","turn:bad@example.org","turns:relay.example:443?transport=tcp"],"username":"alice","credential":"first"}
    ]);
    let selected = select(&servers).unwrap();
    assert_eq!((selected.username, selected.password), ("alice", "first"));
    assert_eq!(
        selected.urls,
        [
            "turn:relay.example:3478?transport=udp",
            "turns:relay.example:443?transport=tcp"
        ]
    );
}

fn selected_after_rejected_entry(rejected: &Value) -> String {
    let servers = json!([
        rejected,
        {"urls":"turns:relay.example:443","username":"caf\u{e9}","credential":"valid"}
    ]);
    let credentials = select(&servers).expect("valid second credential");
    credentials.username.to_string()
}

#[test]
fn skips_credentials_rejected_by_the_grant_wire_format() {
    let stream_safe = format!("x{}", "\u{334}".repeat(30));
    assert!(credential(&json!(stream_safe)).is_some());
    let beyond_stream_safe = stream_safe + "\u{334}";
    for (user, password) in [
        ("cafe\u{301}", "bad"),
        ("first", "cafe\u{301}"),
        (beyond_stream_safe.as_str(), "bad"),
    ] {
        let rejected = json!({"urls":"turn:old.example", "username":user,"credential":password});
        assert_eq!(selected_after_rejected_entry(&rejected), "caf\u{e9}");
    }
}

#[test]
fn skips_hosts_rejected_by_the_grant_wire_format() {
    for host in [
        "bad..example",
        "-bad.example",
        "ab--cd.example",
        "127.1",
        &format!("{}.example", "x".repeat(64)),
    ] {
        let rejected =
            json!({"urls":format!("turn:{host}"), "username":"first","credential":"bad"});
        assert_eq!(
            selected_after_rejected_entry(&rejected),
            "caf\u{e9}",
            "{host}"
        );
    }
}

#[test]
fn preserves_dns_and_ip_spellings_accepted_by_the_grant() {
    for host in [
        "localhost",
        "Relay.Example.",
        "xn--bcher-kva.example",
        "192.0.2.1:3478",
        "[::ffff:192.0.2.1]:443",
    ] {
        assert!(valid_url(&format!("turn:{host}")), "{host}");
    }
}

#[test]
fn limits_unique_urls_and_accepts_a_single_server() {
    let urls: Vec<_> = (0..=MAX_URLS)
        .map(|i| format!("turn:relay-{i}.example"))
        .collect();
    let server = json!({"urls":urls,"username":"user","credential":"password"});
    assert_eq!(select(&server).unwrap().urls, &urls[..MAX_URLS]);
    assert!(
        select(&json!({"urls":"stun:relay.example","username":"user","credential":"password"}))
            .is_none()
    );
}

#[test]
fn key_uses_a_bounded_provider_path_segment_and_has_a_token() {
    let mut key = Key {
        key_id: "Provider_key-1".into(),
        api_token: "token".into(),
    };
    assert!(key.valid());
    key.key_id = "a".repeat(MAX_KEY_ID_BYTES);
    assert!(key.valid());
    for id in [
        "",
        ".",
        "..",
        "x/y",
        "x%2Fy",
        "x y",
        "x\ny",
        &"a".repeat(MAX_KEY_ID_BYTES + 1),
    ] {
        key.key_id = id.to_owned();
        assert!(!key.valid());
    }
    key.key_id = "uid".into();
    key.api_token.clear();
    assert!(!key.valid());
}
