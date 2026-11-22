use super::*;
use serde_json::json;

#[test]
fn refusal_mapping_matches_console_wire_contract() {
    assert_eq!(
        ConsoleError::from_refusal(Refusal::AccessDenied),
        ConsoleError::new(403, "access_denied")
    );
    assert_eq!(
        ConsoleError::from_refusal(Refusal::Cancelled),
        ConsoleError::new(403, "access_denied")
    );
    assert_eq!(
        ConsoleError::from_refusal(Refusal::Expired),
        ConsoleError::new(410, "expired_token")
    );
    assert_eq!(
        ConsoleError::from_refusal(Refusal::Pending),
        ConsoleError::with_retry(202, "authorization_pending", POLL_INTERVAL_MS)
    );
    assert_eq!(
        ConsoleError::from_refusal(Refusal::SlowDown {
            retry_after_ms: 4_200,
        }),
        ConsoleError::with_retry(429, "slow_down", 4_200)
    );
    assert_eq!(
        ConsoleError::from_refusal(Refusal::AlreadyCompleted),
        ConsoleError::new(409, "already_completed")
    );
    assert_eq!(
        ConsoleError::from_refusal(Refusal::InvalidName),
        ConsoleError::new(400, "invalid_name")
    );
    assert_eq!(
        ConsoleError::from_refusal(Refusal::InvalidName).body(),
        json!({"error": "invalid_name"})
    );
    assert_eq!(
        ConsoleError::from_refusal(Refusal::Pending).body(),
        json!({"error": "authorization_pending", "retry_after_ms": POLL_INTERVAL_MS})
    );
}

#[test]
fn mutation_guards_enforce_csrf_and_json_rules() {
    let origin = "https://console.example";
    assert_eq!(
        check_mutation_headers(
            "/api/credentials",
            origin,
            Some("https://evil.example"),
            None,
            Some("application/json"),
        ),
        Err(ConsoleError::forbidden("cross_site_request"))
    );
    assert_eq!(
        check_mutation_headers(
            "/api/enrollments",
            origin,
            None,
            Some("cross-site"),
            Some("application/json"),
        ),
        Err(ConsoleError::forbidden("cross_site_request"))
    );
    assert_eq!(
        check_mutation_headers(
            "/api/credentials",
            origin,
            None,
            None,
            Some("application/json"),
        ),
        Err(ConsoleError::forbidden("origin_required"))
    );
    assert_eq!(
        check_mutation_headers(
            "/api/enrollments",
            origin,
            None,
            None,
            Some("application/json; charset=utf-8"),
        ),
        Ok(())
    );
    assert_eq!(
        check_mutation_headers(
            "/api/credentials",
            origin,
            Some(origin),
            Some("same-origin"),
            Some(" application/json ; charset=utf-8 "),
        ),
        Ok(())
    );
    assert_eq!(
        check_mutation_headers(
            "/api/credentials",
            origin,
            Some(origin),
            None,
            Some("text/plain"),
        ),
        Err(ConsoleError::new(415, "json_required"))
    );
}

#[test]
fn cookies_and_field_parsers_handle_edge_cases() {
    assert_eq!(
        extract_cookie("other=1; __Host-admin=tok_123; third=2", "__Host-admin"),
        Some("tok_123")
    );
    assert_eq!(extract_cookie("other=1", "__Host-admin"), None);
    assert_eq!(
        format_session_cookie("__Host-admin", "secret", "/", true),
        format!(
            "__Host-admin=secret; Path=/; HttpOnly; Secure; SameSite=Strict; Max-Age={}",
            ADMIN_SESSION_LIFETIME_MS / 1_000
        )
    );
    assert_eq!(
        format_session_cookie("admin", "", "/api", false),
        "admin=; Path=/api; HttpOnly; SameSite=Strict; Max-Age=0"
    );

    let encoded_16 = crate::text_id(&[5; 16]);
    let body = json!({"id": encoded_16, "flag": true, "bad_bool": "yes"});
    assert_eq!(required_id::<16>(&body, "id"), Ok([5; 16]));
    assert!(required_id::<32>(&body, "id").is_err());
    assert_eq!(optional_bool(&body, "flag"), Ok(true));
    assert_eq!(optional_bool(&body, "absent"), Ok(false));
    assert_eq!(
        optional_bool(&body, "bad_bool"),
        Err(ConsoleError::bad_request("invalid_request"))
    );
}
