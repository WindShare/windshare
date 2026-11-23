use super::*;
use crate::node::access::{Authorization, Bearer, Policy};
use std::path::Path;
use windshare_relay_access::{Credential, CredentialState};
use windshare_relay_console::{
    ADMIN_SESSION_LIFETIME_MS, MAX_ACTIVE_CREDENTIALS, credentials::CREDENTIAL_HISTORY_PAGE_SIZE,
    text_id,
};
use windshare_relay_core::identifiers::ShareId;

fn open(path: &Path, identity: &Identity, policy: &Policy) -> Console {
    let endpoint = Endpoint::parse("wss://node.example/v2/ws").unwrap();
    let db = Database::open(path, endpoint.identity).unwrap();
    let access = Service::default();
    access.configure(policy.clone()).unwrap();
    Console::open(&endpoint, &db, access, identity, None)
        .unwrap()
        .unwrap()
}

fn post(console: &Console, path: &str, body: &Value, session: &str) -> Reply {
    console
        .dispatch(&Method::POST, path, body, session, 100)
        .unwrap()
}

fn revoke(console: &Console, session: &str, id: [u8; 16]) -> StatusCode {
    post(
        console,
        "/api/credentials/revoke",
        &json!({"credential_id":text_id(&id)}),
        session,
    )
    .status
}

fn pair(console: &Console, session: &str, secret: [u8; 32]) -> String {
    let req = post(
        console,
        "/api/enrollments",
        &json!({
            "device_name":"Laptop","claim_digest":text_id(&Digest::of(&secret).0)
        }),
        "",
    );
    let approve = post(console, "/api/enrollments/approve", &req.body, session);
    assert_eq!(approve.status, StatusCode::OK);
    let claim = json!({"enrollment_id":req.body["enrollment_id"],"claim_secret":text_id(&secret)});
    let credential = post(console, "/api/enrollments/claim", &claim, "");
    let key = credential.body["access_key"].as_str().unwrap();
    assert!(!admitted(console, key));
    let install = || post(console, "/api/enrollments/installed", &claim, "").status;
    assert_eq!(install(), StatusCode::OK);
    assert!(admitted(console, key));
    let changes = console.0.access.changes();
    assert_eq!(install(), StatusCode::OK);
    assert!(!changes.has_changed().unwrap());
    key.to_owned()
}

fn admitted(console: &Console, secret: &str) -> bool {
    matches!(
        console.0.access.authorize(
            ShareId::new([1; 12]).unwrap(),
            Some(&Bearer::new(secret)),
            100
        ),
        Authorization::Granted { .. }
    )
}

#[test]
fn revoking_a_credential_makes_room_for_a_new_device() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("node.sqlite");
    let identity = Identity::generate().unwrap();
    let console = open(&path, &identity, &Policy::default());
    let setup = json!({"token":identity.initialization_token(),"password":"development-password"});
    let session = post(&console, "/api/initialize", &setup, "")
        .cookie
        .unwrap();
    {
        let mut db = console.0.db.lock().unwrap();
        let tx = db.transaction().unwrap();
        for i in 0..=MAX_ACTIVE_CREDENTIALS + CREDENTIAL_HISTORY_PAGE_SIZE {
            let state = match i.cmp(&MAX_ACTIVE_CREDENTIALS) {
                std::cmp::Ordering::Less => CredentialState::Active,
                std::cmp::Ordering::Equal => CredentialState::Revoked { at: 1 },
                std::cmp::Ordering::Greater => CredentialState::Revoked { at: 2 },
            };
            database::save_key(
                &tx,
                &Credential {
                    id: (i as u128).to_le_bytes(),
                    principal: [4; 16],
                    name: "Existing key".into(),
                    digest: Digest::of(i.to_string().as_bytes()),
                    created_at: 1,
                    last_used_at: None,
                    state,
                },
            )
            .unwrap();
        }
        let keys = database::keys(&tx).unwrap();
        tx.commit().unwrap();
        drop(db);
        console.0.access.restore_devices(keys);
    }
    let get = |path: &str| {
        console
            .dispatch(&Method::GET, path, &json!({}), &session, 100)
            .unwrap()
    };
    let active = get("/api/credentials");
    let active = active.body["credentials"].as_array().unwrap();
    assert_eq!(active.len(), MAX_ACTIVE_CREDENTIALS);
    assert!(active.iter().all(|entry| entry["revoked_at"].is_null()));
    let page = get("/api/credentials/revoked");
    assert_eq!(page.status, StatusCode::OK);
    let history = page.body["credentials"].as_array().unwrap();
    assert_eq!(history.len(), CREDENTIAL_HISTORY_PAGE_SIZE);
    assert!(history.iter().all(|entry| entry["revoked_at"] == 2));
    let cursor = page.body["next_cursor"].as_str().unwrap();
    let next = get(&format!("/api/credentials/revoked?after={cursor}"));
    let older = next.body["credentials"].as_array().unwrap();
    assert_eq!(older.len(), 1);
    assert_eq!(older[0]["revoked_at"], 1);
    assert!(next.body["next_cursor"].is_null());
    assert_eq!(
        get("/api/credentials/revoked?after=invalid").status,
        StatusCode::BAD_REQUEST
    );

    let create = |name| {
        post(
            &console,
            "/api/credentials",
            &json!({"name": name}),
            &session,
        )
    };
    assert_eq!(
        create("Over capacity").status,
        StatusCode::TOO_MANY_REQUESTS
    );
    let id = 1u128.to_le_bytes();
    assert!(admitted(&console, "1"));
    assert_eq!(revoke(&console, &session, id), StatusCode::OK);
    assert!(!admitted(&console, "1"));
    let changes = console.0.access.changes();
    assert_eq!(revoke(&console, &session, id), StatusCode::OK);
    assert!(!changes.has_changed().unwrap());
    pair(&console, &session, [25; 32]);
    assert_eq!(
        create("Over capacity again").status,
        StatusCode::TOO_MANY_REQUESTS
    );
}

#[test]
fn revocation_survives_reload_and_restart_without_affecting_other_owner_devices() {
    let dir = tempfile::tempdir().unwrap();
    let db = dir.path().join("node.sqlite");
    let identity = Identity::generate().unwrap();
    let configured = Credential {
        id: [3; 16],
        principal: [4; 16],
        name: "Configured".into(),
        digest: Digest::of(b"configured-key"),
        created_at: 1,
        last_used_at: None,
        state: CredentialState::Active,
    };
    let mut policy = Policy {
        anonymous: false,
        owner_principals: vec![identity.owner().principal],
        credentials: vec![configured.clone()],
        ..Policy::default()
    };
    let console = open(&db, &identity, &policy);
    let setup = json!({"token":identity.initialization_token(),"password":"development-password"});
    let session = post(&console, "/api/initialize", &setup, "")
        .cookie
        .unwrap();
    assert_eq!(
        post(&console, "/api/initialize", &setup, "").status,
        StatusCode::FORBIDDEN
    );
    let first = pair(&console, &session, [1; 32]);
    let second = pair(&console, &session, [2; 32]);
    let inventory = console.0.access.active_credentials();
    let device = |key: &str| {
        inventory
            .iter()
            .find(|c| c.digest == Digest::of(key.as_bytes()))
            .unwrap()
    };
    assert_eq!(device(&first).principal, identity.owner().principal);
    assert_eq!(device(&second).principal, device(&first).principal);
    assert_ne!(device(&second).id, device(&first).id);
    let manual = post(
        &console,
        "/api/credentials",
        &json!({"name":"automation"}),
        &session,
    );
    assert_eq!(manual.status, StatusCode::CREATED);
    let another = post(
        &console,
        "/api/credentials",
        &json!({"name":"second"}),
        &session,
    );
    assert_ne!(
        manual.body["credential"]["principal_id"],
        another.body["credential"]["principal_id"]
    );
    assert_ne!(
        manual.body["credential"]["principal_id"],
        text_id(&device(&first).principal)
    );
    for id in [device(&first).id, configured.id] {
        assert_eq!(revoke(&console, &session, id), StatusCode::OK);
    }
    policy.credentials[0].id = [7; 16];
    console.0.access.configure(policy.clone()).unwrap();
    assert!(!admitted(&console, "configured-key"));
    drop(console);
    let restored = open(&db, &identity, &policy);
    assert!(!admitted(&restored, &first));
    assert!(!admitted(&restored, "configured-key"));
    assert!(admitted(&restored, &second));
    assert!(admitted(
        &restored,
        manual.body["access_key"].as_str().unwrap()
    ));
}

#[test]
fn password_change_rotates_sessions_and_advertises_capability() {
    let dir = tempfile::tempdir().unwrap();
    let db = dir.path().join("node.sqlite");
    let identity = Identity::generate().unwrap();
    let console = open(&db, &identity, &Policy::default());
    let setup =
        json!({"token":identity.initialization_token(),"password":"initial-owner-password"});
    let first = post(&console, "/api/initialize", &setup, "")
        .cookie
        .unwrap();
    let second = post(
        &console,
        "/api/login",
        &json!({"password":"initial-owner-password"}),
        "",
    )
    .cookie
    .unwrap();
    let session_info = console
        .dispatch(&Method::GET, "/api/session", &json!({}), &first, 100)
        .unwrap();
    assert_eq!(session_info.body["capabilities"]["password_change"], true);
    assert_eq!(
        post(
            &console,
            "/api/password",
            &json!({"current_password":"wrong-password","new_password":"rotated-owner-password"}),
            &first,
        )
        .status,
        StatusCode::FORBIDDEN
    );
    assert_eq!(
        post(
            &console,
            "/api/password",
            &json!({"current_password":"initial-owner-password","new_password":"short"}),
            &first,
        )
        .status,
        StatusCode::BAD_REQUEST
    );
    let changed = post(
        &console,
        "/api/password",
        &json!({"current_password":"initial-owner-password","new_password":"rotated-owner-password"}),
        &first,
    );
    assert_eq!(changed.status, StatusCode::OK);
    let replacement = changed.cookie.unwrap();
    for stale in [&first, &second] {
        assert_eq!(
            console
                .dispatch(&Method::GET, "/api/session", &json!({}), stale, 100)
                .unwrap()
                .status,
            StatusCode::UNAUTHORIZED
        );
    }
    assert_eq!(
        console
            .dispatch(&Method::GET, "/api/session", &json!({}), &replacement, 100)
            .unwrap()
            .status,
        StatusCode::OK
    );
    assert_eq!(
        post(
            &console,
            "/api/login",
            &json!({"password":"initial-owner-password"}),
            "",
        )
        .status,
        StatusCode::UNAUTHORIZED
    );
    assert_eq!(
        post(
            &console,
            "/api/login",
            &json!({"password":"rotated-owner-password"}),
            "",
        )
        .status,
        StatusCode::OK
    );
}

#[test]
fn admin_session_cookie_and_lifetime_match_shared_console_policy() {
    let dir = tempfile::tempdir().unwrap();
    let db = dir.path().join("node.sqlite");
    let identity = Identity::generate().unwrap();
    let console = open(&db, &identity, &Policy::default());
    let setup = json!({"token":identity.initialization_token(),"password":"development-password"});
    let signed_in = post(&console, "/api/initialize", &setup, "");
    let session = signed_in.cookie.clone().unwrap();
    let set_cookie = console
        .render(signed_in)
        .headers()
        .get(hyper::header::SET_COOKIE)
        .unwrap()
        .to_str()
        .unwrap()
        .to_owned();
    assert!(
        set_cookie.contains(&format!(
            "Path=/api; HttpOnly; Secure; SameSite=Strict; Max-Age={}",
            ADMIN_SESSION_LIFETIME_MS / 1_000
        )),
        "{set_cookie}"
    );
    assert_eq!(
        console
            .dispatch(
                &Method::GET,
                "/api/session",
                &json!({}),
                &session,
                100 + ADMIN_SESSION_LIFETIME_MS - 1,
            )
            .unwrap()
            .status,
        StatusCode::OK
    );
    let logged_out = console
        .dispatch(
            &Method::POST,
            "/api/logout",
            &json!({}),
            &session,
            100 + ADMIN_SESSION_LIFETIME_MS - 1,
        )
        .unwrap();
    assert_eq!(logged_out.status, StatusCode::OK);
    let cleared_cookie = console
        .render(logged_out)
        .headers()
        .get(hyper::header::SET_COOKIE)
        .unwrap()
        .to_str()
        .unwrap()
        .to_owned();
    assert!(cleared_cookie.contains("Max-Age=0"), "{cleared_cookie}");
    let re_signed_in = post(
        &console,
        "/api/login",
        &json!({"password":"development-password"}),
        "",
    );
    let next_session = re_signed_in.cookie.unwrap();
    assert_eq!(
        console
            .dispatch(
                &Method::GET,
                "/api/session",
                &json!({}),
                &next_session,
                100 + ADMIN_SESSION_LIFETIME_MS,
            )
            .unwrap()
            .status,
        StatusCode::UNAUTHORIZED
    );
}

#[test]
fn native_console_aligns_error_codes_and_enrollment_payload_contract() {
    let dir = tempfile::tempdir().unwrap();
    let db = dir.path().join("node.sqlite");
    let identity = Identity::generate().unwrap();
    let console = open(&db, &identity, &Policy::default());
    let setup = json!({"token":identity.initialization_token(),"password":"development-password"});
    let session = post(&console, "/api/initialize", &setup, "")
        .cookie
        .unwrap();

    let invalid = post(
        &console,
        "/api/credentials",
        &json!({"name":"   "}),
        &session,
    );
    assert_eq!(invalid.status, StatusCode::BAD_REQUEST);
    assert_eq!(invalid.body["error"], "invalid_name");

    let secret = [17_u8; 32];
    let enrolled = post(
        &console,
        "/api/enrollments",
        &json!({
            "device_name":"Workstation",
            "claim_digest":text_id(&Digest::of(&secret).0),
        }),
        "",
    );
    assert_eq!(enrolled.status, StatusCode::OK);
    let pending = console
        .dispatch(&Method::GET, "/api/enrollments", &json!({}), &session, 100)
        .unwrap();
    assert_eq!(pending.body["enrollments"][0]["created_at"], 100);

    assert_eq!(
        post(
            &console,
            "/api/enrollments/approve",
            &enrolled.body,
            &session
        )
        .status,
        StatusCode::OK
    );
    let after_approve = console
        .dispatch(&Method::GET, "/api/enrollments", &json!({}), &session, 100)
        .unwrap();
    assert_eq!(after_approve.body, json!({"enrollments": []}));
    let claim_body = json!({
        "enrollment_id": enrolled.body["enrollment_id"],
        "claim_secret": text_id(&secret),
    });
    let claimed = post(&console, "/api/enrollments/claim", &claim_body, "");
    assert_eq!(claimed.status, StatusCode::OK);
    assert_eq!(claimed.body["expires_at"], enrolled.body["expires_at"]);
    let key = claimed.body["access_key"].as_str().unwrap();

    let installed = post(&console, "/api/enrollments/installed", &claim_body, "");
    assert_eq!(installed.status, StatusCode::OK);
    assert!(installed.body.get("managed_turn").is_none());
    assert!(admitted(&console, key));

    let next_secret = [18_u8; 32];
    let next_enrolled = post(
        &console,
        "/api/enrollments",
        &json!({
            "device_name":"Laptop",
            "claim_digest":text_id(&Digest::of(&next_secret).0),
        }),
        "",
    );
    assert_eq!(next_enrolled.status, StatusCode::OK);
    let visible = console
        .dispatch(&Method::GET, "/api/enrollments", &json!({}), &session, 100)
        .unwrap();
    assert_eq!(visible.body["enrollments"].as_array().unwrap().len(), 1);
    assert_eq!(
        visible.body["enrollments"][0]["enrollment_id"],
        next_enrolled.body["enrollment_id"]
    );

    assert_eq!(
        post(&console, "/api/enrollments/cancel", &claim_body, "").status,
        StatusCode::OK
    );
    assert!(!admitted(&console, key));
}
