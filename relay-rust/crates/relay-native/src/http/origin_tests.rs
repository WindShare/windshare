use super::*;

fn headers(origin: &str) -> HeaderMap {
    let mut headers = HeaderMap::new();
    headers.insert(header::ORIGIN, origin.parse().unwrap());
    headers
}

#[test]
fn native_clients_are_distinct_from_empty_opaque_or_multiple_origins() {
    let policy = OriginPolicy::default();
    assert!(policy.allows(&HeaderMap::new()));
    for origin in ["", "null", "https://app.example", "http://localhost:5173"] {
        assert!(!policy.allows(&headers(origin)), "{origin}");
    }
    let policy = OriginPolicy::new(["https://app.example"], false).unwrap();
    let mut repeated = headers("https://app.example");
    repeated.append(header::ORIGIN, "https://app.example".parse().unwrap());
    assert!(!policy.allows(&repeated));
    assert!(!policy.allows(&headers("https://app.example,https://app.example")));
    assert!(!policy.allows(&headers("https://app.example https://app.example")));
}

#[test]
fn allowlist_matches_the_complete_normalized_origin() {
    let policy = OriginPolicy::new(
        ["HTTPS://App.Example:443", "http://[0:0:0:0:0:0:0:1]:8080"],
        false,
    )
    .unwrap();
    for origin in [
        "https://app.example",
        "https://APP.EXAMPLE:443",
        "http://[::1]:8080",
    ] {
        assert!(policy.allows(&headers(origin)), "{origin}");
    }
    for origin in [
        "http://app.example",
        "https://app.example:8443",
        "https://sub.app.example",
        "https://app.example.evil",
        "https://app.example.",
        "http://[::1]:8081",
        "http://localhost:8080",
    ] {
        assert!(!policy.allows(&headers(origin)), "{origin}");
    }
}

#[test]
fn development_policy_only_allows_explicit_loopback_hosts() {
    let policy = OriginPolicy::new(std::iter::empty::<&str>(), true).unwrap();
    for origin in [
        "http://localhost:5173",
        "https://LOCALHOST",
        "http://127.0.0.1:80",
        "http://[::1]:5173",
    ] {
        assert!(policy.allows(&headers(origin)), "{origin}");
    }
    for origin in [
        "http://localhost.evil",
        "http://localhost.",
        "http://dev.localhost",
        "http://127.0.0.2",
        "http://[::ffff:127.0.0.1]",
        "http://192.168.1.2",
        "http://0.0.0.0",
        "http://127.1",
    ] {
        assert!(!policy.allows(&headers(origin)), "{origin}");
    }
}

#[test]
fn invalid_configuration_and_request_origins_are_rejected_consistently() {
    let policy = OriginPolicy::new(["https://app.example"], true).unwrap();
    for origin in [
        "",
        "null",
        "*",
        "https://*.example",
        "https://app.example/",
        "https://app.example/path",
        "https://app.example?",
        "https://app.example#",
        "https://user@app.example",
        "file://localhost",
        "ws://localhost:5173",
        "https://app.example:0",
        "https://app.example:",
        "https://app.example:65536",
        "https://app.example\\evil",
        "https://%61pp.example",
        " https://app.example",
        "https://app.example ",
        "https://2130706433",
        "https://127.1",
    ] {
        assert!(OriginPolicy::new([origin], false).is_err(), "{origin}");
        assert!(!policy.allows(&headers(origin)), "{origin}");
    }
}
