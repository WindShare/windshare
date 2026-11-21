use super::*;
use crate::node::access::{
    AuthorityState, Authorization, Bearer, Policy, Refusal, RefusalReason, TrustedIssuer,
};
use ed25519_dalek::SigningKey;
use windshare_relay_access::{
    Attribution,
    device_login::{Approval, Invitation},
    entitlement::Entitlement,
    trust::Generation,
    usage::NodePolicy,
};
use windshare_relay_console::text_id;
use windshare_relay_core::{access::RegistrationDecision, identifiers::ShareId};

fn open_console(
    endpoint: &Endpoint,
    db: &Database,
    identity: &Identity,
    generation: Generation,
    root: &SigningKey,
) -> Console {
    let access = Service::default();
    let service = crate::node::usage::Authority {
        url: "https://service.example".into(),
        issuer: text_id(&generation.issuer),
    };
    access
        .configure(Policy {
            trusted_issuers: vec![TrustedIssuer {
                root: service.issuer.clone(),
                certificate: text_id(&generation.certify(root).unwrap()),
            }],
            ..Policy::default()
        })
        .unwrap();
    access.set_controller(Some(generation.issuer));
    access
        .apply_authority(AuthorityState {
            generation,
            policy: Some(NodePolicy {
                version: 1,
                enabled: true,
                anonymous: false,
            }),
        })
        .unwrap();
    let console = Console::open(endpoint, db, access.clone(), identity, Some(service))
        .unwrap()
        .unwrap();
    assert!(console.login_page(10).is_none());
    access.observe_device_login(generation.issuer, true);
    assert_eq!(
        console.login_page(10).as_deref(),
        Some("https://service.example/device")
    );
    console
}

fn admitted_as(console: &Console, share: ShareId, key: &Bearer, account: Attribution) -> bool {
    matches!(
        console.0.access.authorize(share, Some(key), 11),
        Authorization::Granted { attribution, .. } if attribution == account
    )
}

#[test]
fn service_approval_installs_an_account_key_and_node_revocation_survives_restart() {
    let dir = tempfile::tempdir().unwrap();
    let endpoint = Endpoint::parse("wss://relay.example/v2/ws").unwrap();
    let db = Database::open(&dir.path().join("node.sqlite"), endpoint.identity).unwrap();
    let identity = Identity::generate().unwrap();
    let signing = SigningKey::from_bytes(&[42; 32]);
    let root = SigningKey::from_bytes(&[41; 32]);
    let generation = Generation {
        issuer: root.verifying_key().to_bytes(),
        key: signing.verifying_key().to_bytes(),
        number: 1,
        not_before: 0,
        expires_at: 3600,
    };
    let setup = || open_console(&endpoint, &db, &identity, generation, &root);
    let console = setup();
    let post = |path, body: &Value| {
        console
            .dispatch(&Method::POST, path, body, "", 10000)
            .unwrap()
    };
    let secret = [9; 32];
    let req = post(
        "/api/enrollments",
        &json!({"device_name":"Laptop","claim_digest":text_id(&Digest::of(&secret).0)}),
    );
    let invite = Invitation::verify(
        req.body["account_invitation"].as_str().unwrap(),
        generation.issuer,
        10000,
    )
    .unwrap();
    let account = [7; 16];
    let grant = Approval {
        node: identity.key().verifying_key().to_bytes(),
        issuer: generation.issuer,
        enrollment: invite.enrollment,
        account,
        expires_ms: invite.expires_ms,
        entitlement: Entitlement {
            version: 1,
            day: 0,
            valid_until: 300,
            remaining_bytes: 100,
        },
    };
    let approve = json!({"approval":grant.issue(&signing).unwrap()});
    for _ in 0..2 {
        assert_eq!(
            post("/api/enrollments/account", &approve).status,
            StatusCode::OK
        );
    }
    let claim = json!({"enrollment_id":req.body["enrollment_id"],"claim_secret":text_id(&secret)});
    let claimed = post("/api/enrollments/claim", &claim);
    let key = Bearer::new(claimed.body["access_key"].as_str().unwrap());
    assert_eq!(
        post("/api/enrollments/installed", &claim).status,
        StatusCode::OK
    );
    let attribution = Attribution::Account {
        issuer: generation.issuer,
        account,
        credential: invite.enrollment,
    };
    let share = ShareId::new([1; 12]).unwrap();
    assert!(admitted_as(&console, share, &key, attribution));
    drop(console);
    let restored = setup();
    assert!(admitted_as(&restored, share, &key, attribution));
    let admin = restored
        .dispatch(
            &Method::POST,
            "/api/initialize",
            &json!({"token":identity.initialization_token(),"password":"owner-only-password"}),
            "",
            11000,
        )
        .unwrap()
        .cookie
        .unwrap();
    restored
        .dispatch(
            &Method::POST,
            "/api/credentials/revoke",
            &json!({"credential_id":text_id(&invite.enrollment)}),
            &admin,
            11000,
        )
        .unwrap();
    assert!(!restored.0.access.allowed(share, attribution));
    drop(restored);
    assert!(matches!(
        setup().0.access.authorize(share, Some(&key), 12),
        Authorization::Refused(Refusal {
            decision: RegistrationDecision::Denied,
            reason: RefusalReason::CredentialInactive,
        })
    ));
}
