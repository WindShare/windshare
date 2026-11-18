use super::*;
use crate::node::access::Policy;
use std::path::Path;
use windshare_relay_access::{Credential, CredentialState};
use windshare_relay_console::text_id;
use windshare_relay_core::{access::RegistrationDecision, identifiers::ShareId};

fn open(path: &Path, identity: &Identity, policy: &Policy) -> Console {
    let endpoint = Endpoint::parse("wss://node.example/v2/ws").unwrap();
    let db = Database::open(path, endpoint.identity).unwrap();
    let access = Service::default();
    access.configure(policy.clone()).unwrap();
    Console::open(&endpoint, &db, access, identity)
        .unwrap()
        .unwrap()
}

fn post(console: &Console, path: &str, body: &Value, session: &str) -> Reply {
    console
        .dispatch(&Method::POST, path, body, session, 100)
        .unwrap()
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
    for _ in 0..2 {
        assert_eq!(
            post(console, "/api/enrollments/installed", &claim, "").status,
            StatusCode::OK
        );
    }
    assert!(admitted(console, key));
    key.to_owned()
}

fn admitted(console: &Console, secret: &str) -> bool {
    matches!(
        console
            .0
            .access
            .authorize(ShareId::new([1; 12]).unwrap(), Some(secret), 100),
        RegistrationDecision::Granted(_)
    )
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
    let inventory = console.0.access.credential_inventory();
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
        assert_eq!(
            post(
                &console,
                "/api/credentials/revoke",
                &json!({"credential_id":text_id(&id)}),
                &session
            )
            .status,
            StatusCode::OK
        );
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
