use super::*;
use sha2::{Digest, Sha256};
use windshare_relay_access::{AccountId, device_login::Invitation};

fn fault_of<T: std::fmt::Debug>(result: Result<T>) -> Fault {
    result.unwrap_err().downcast::<Fault>().unwrap()
}

fn seed_password_account(
    authority: &mut Authority<Memory, Clock>,
    secret_byte: u8,
    username: &str,
    verifier: &PasswordVerifier,
) -> (AccountId, String) {
    let request = registration(secret_byte);
    let secret = request.client_secret.clone();
    let source = format!("seed-{secret_byte}");
    let session = authority.register(&source, request).unwrap();
    authority
        .set_password(&source, &secret, username, verifier)
        .unwrap();
    (session.account, secret)
}

#[test]
fn hmac_sha256_matches_rfc4231_test_vector() {
    // RFC 4231 Test Case 4 with 25-byte key (zero-padded to 32 bytes is equivalent to HMAC's 64-byte block padding).
    let mut key = [0_u8; 32];
    for (idx, slot) in key.iter_mut().take(25).enumerate() {
        *slot = u8::try_from(idx + 1).unwrap();
    }
    let data = [0xcd_u8; 50];
    let mac = crate::password::hmac_sha256(&key, &[&data[..17], &data[17..]]);
    let expected: [u8; 32] = [
        0x82, 0x55, 0x8a, 0x38, 0x9a, 0x44, 0x3c, 0x0e, 0xa4, 0xcc, 0x81, 0x98, 0x99, 0xf2, 0x08,
        0x3a, 0x85, 0xf0, 0xfa, 0xa3, 0xe5, 0x78, 0xf8, 0x07, 0x7a, 0x2e, 0x3f, 0xf4, 0x67, 0x29,
        0x66, 0x5b,
    ];
    assert_eq!(mac, expected);
}

#[test]
fn pepper_ring_validates_format_supports_rotation_and_redacts_secrets() {
    let v1_key = URL_SAFE_NO_PAD.encode([0x11_u8; 32]);
    let v2_key = URL_SAFE_NO_PAD.encode([0x22_u8; 32]);
    let v1_only = PepperRing::parse(&format!("v1:{v1_key}")).unwrap();
    let rotated = PepperRing::parse(&format!("v2:{v2_key},v1:{v1_key}")).unwrap();
    let v2_only = PepperRing::parse(&format!("v2:{v2_key}")).unwrap();

    let verifier = test_verifier("correct horse battery");
    let wrong = test_verifier("different password");
    let stored_v1 = StoredPassword::seal(&verifier, [3; PASSWORD_SALT_BYTES], &v1_only);
    assert_eq!(stored_v1.pepper_id(), "v1");
    assert!(stored_v1.verify(&verifier, &v1_only));
    assert!(stored_v1.verify(&verifier, &rotated));
    assert!(!stored_v1.verify(&wrong, &rotated));
    assert!(!stored_v1.verify(&verifier, &v2_only));

    let stored_v2 = StoredPassword::seal(&verifier, [3; PASSWORD_SALT_BYTES], &rotated);
    assert_eq!(stored_v2.pepper_id(), "v2");
    assert_ne!(stored_v1, stored_v2);
    assert!(stored_v2.verify(&verifier, &rotated));
    assert!(stored_v2.verify(&verifier, &v2_only));

    // Debug representations must never expose verifier or MAC bytes.
    let verifier_wire = String::from(verifier.clone());
    assert!(!format!("{verifier:?}").contains(&verifier_wire));
    let stored_wire = String::from(stored_v1.clone());
    assert!(!format!("{stored_v1:?}").contains(&stored_wire));
    assert!(!format!("{rotated:?}").contains(&v1_key));
    assert!(!format!("{rotated:?}").contains(&v2_key));

    // Malformed peppers and stored/verifier encodings fail closed.
    let zeros = URL_SAFE_NO_PAD.encode([0_u8; 32]);
    for bad in [
        "",
        "missing-colon",
        &format!("BAD:{v1_key}"),
        &format!("v1:{v1_key},v1:{v2_key}"),
        "v1:short",
        &format!("v1:{zeros}"),
    ] {
        assert!(PepperRing::parse(bad).is_err(), "{bad}");
    }
    assert!(PasswordVerifier::try_from(String::from("plaintext-password")).is_err());
    assert!(StoredPassword::try_from(String::from("plaintext-password")).is_err());
    assert!(StoredPassword::try_from(String::from("v1$bad$bad")).is_err());
}

#[test]
fn shared_password_verifier_vectors_parse_and_verify_with_domain_separation() {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../../testdata/network/password-verifiers.json");
    let fixture_json: serde_json::Value =
        serde_json::from_slice(&std::fs::read(path).unwrap()).unwrap();
    let vectors = fixture_json["vectors"].as_array().unwrap();
    assert!(vectors.len() >= 3);

    let mut authority = fixture();
    let account_vector = &vectors[0];
    let admin_vector = &vectors[1];
    let account_verifier =
        PasswordVerifier::try_from(account_vector["verifier"].as_str().unwrap().to_owned())
            .unwrap();
    let admin_verifier =
        PasswordVerifier::try_from(admin_vector["verifier"].as_str().unwrap().to_owned()).unwrap();
    assert_ne!(account_verifier, admin_verifier);

    let username = account_vector["username"].as_str().unwrap();
    authority
        .initialize_admin("init", "init", username, &admin_verifier)
        .unwrap();
    let req = registration(40);
    let secret = req.client_secret.clone();
    authority.register("vector-ip", req).unwrap();
    authority
        .set_password("vector-ip", &secret, username, &account_verifier)
        .unwrap();

    // Admin verifier cannot sign into the account, and account verifier cannot sign into admin.
    assert!(
        authority
            .login("vector-ip", username, &admin_verifier)
            .is_err()
    );
    assert!(
        authority
            .admin_login("vector-ip", username, &account_verifier)
            .is_err()
    );
    assert!(
        authority
            .login("vector-ip", username, &account_verifier)
            .is_ok()
    );
    assert!(
        authority
            .admin_login("vector-ip", username, &admin_verifier)
            .is_ok()
    );
}

#[test]
fn username_normalization_uses_ascii_lowering_and_trims_unicode_whitespace() {
    assert_eq!(
        crate::registration::username("\u{00A0}\u{3000}Alice_1.0\u{3000}\u{00A0}").unwrap(),
        "alice_1.0"
    );
    for invalid in ["\u{212A}evin", "\u{0130}van"] {
        assert!(matches!(
            fault_of(crate::registration::username(invalid)),
            Fault::Invalid
        ));
    }
}

#[test]
fn independent_random_salts_prevent_cookie_resurrection() {
    let mut authority = fixture();
    let original = test_verifier("correct horse battery");
    let replacement = test_verifier("another valid password");
    let secret = registration(1).client_secret;
    let session = authority
        .register_with_password("source", registration(1), "alice", "correct horse battery")
        .unwrap();
    let initial_account = load::<Account>(&authority.store, &key("account", &session.account))
        .unwrap()
        .unwrap();
    let initial_stored = initial_account.password.unwrap();

    let signer = SigningKey::from_bytes(&[9; 32]);
    let node = signer.verifying_key().to_bytes();
    authority.seed_node(node, Zone::Global, "test");
    let invite = Invitation {
        node,
        issuer: authority.signing.generation.issuer,
        enrollment: [3; 16],
        origin: "https://relay.example".into(),
        device: "Desktop".into(),
        code: "1234-5678".into(),
        expires_ms: authority.environment.now() * 1000 + 60_000,
    }
    .issue(&signer)
    .unwrap();
    let cookie = authority
        .device_sign_in("source", &invite, "alice", &original)
        .unwrap()
        .session;
    assert!(authority.browser_account(&cookie).is_some());

    // Changing A -> B -> A must produce a fresh salt and keep the old cookie invalid.
    authority
        .set_password("source", &secret, "alice", &replacement)
        .unwrap();
    authority
        .set_password("source", &secret, "alice", &original)
        .unwrap();
    let final_stored = load::<Account>(&authority.store, &key("account", &session.account))
        .unwrap()
        .unwrap()
        .password
        .unwrap();
    assert_ne!(initial_stored, final_stored);
    assert!(final_stored.verify(&original, &authority.environment.peppers));
    assert!(authority.browser_account(&cookie).is_none());
}

#[test]
fn password_change_limits_atomically_without_burning_global_on_invalid_input() {
    let mut authority = fixture();
    let verifier = test_verifier("correct horse battery");
    let (_, secret_a) = seed_password_account(&mut authority, 1, "alice", &verifier);
    let (_, secret_b) = seed_password_account(&mut authority, 2, "bob", &verifier);
    // Refill the buckets touched while seeding alice and bob so the test starts from full limits.
    authority.environment.now += 60;
    let writes_before_invalid = authority.store.put_count;

    // Invalid usernames and taken usernames do not spend account or global quotas.
    for _ in 0..10 {
        assert!(matches!(
            fault_of(authority.set_password("typo-ip", &secret_a, "bad user", &verifier)),
            Fault::Invalid
        ));
        assert!(matches!(
            fault_of(authority.set_password("typo-ip", &secret_a, "bob", &verifier)),
            Fault::Denied { reason } if reason == "username_taken"
        ));
    }
    assert_eq!(authority.store.put_count, writes_before_invalid);

    // Invalid credentials count against only the caller's source bucket.
    for _ in 0..5 {
        assert!(matches!(
            fault_of(authority.set_password("bad-ip", "wrong-secret", "alice", &verifier)),
            Fault::Denied { reason } if reason == "credentials"
        ));
    }
    assert!(matches!(
        fault_of(authority.set_password("bad-ip", &secret_a, "alice", &verifier)),
        Fault::RateLimited { retry_after: 60 }
    ));

    // Rotating source IPs hits the per-account bucket after 5 attempts.
    for i in 0..5 {
        authority
            .set_password(&format!("ip-a-{i}"), &secret_a, "alice", &verifier)
            .unwrap();
    }
    assert!(matches!(
        fault_of(authority.set_password("ip-a-extra", &secret_a, "alice", &verifier)),
        Fault::RateLimited { retry_after: 60 }
    ));

    // Exhaust the remaining global password-change burst (60 total; 5 spent above).
    let mut spent = 5;
    for acct_idx in 3..=20u8 {
        let req = registration(acct_idx);
        let secret = req.client_secret.clone();
        authority
            .register(&format!("seed-{acct_idx}"), req)
            .unwrap();
        for attempt in 0..5 {
            let source = format!("global-{acct_idx}-{attempt}");
            let result =
                authority.set_password(&source, &secret, &format!("user{acct_idx}"), &verifier);
            if spent < 60 {
                result.unwrap();
                spent += 1;
            } else {
                // Global exhaustion must not drain bob's untouched source or account bucket.
                assert!(matches!(
                    fault_of(authority.set_password("innocent-ip", &secret_b, "bob", &verifier)),
                    Fault::RateLimited { retry_after: 1 }
                ));
                assert!(matches!(
                    fault_of(result),
                    Fault::RateLimited { retry_after: 1 }
                ));
                authority.environment.now += 1;
                for _ in 0..5 {
                    authority.environment.now += 1;
                    authority
                        .set_password("innocent-ip", &secret_b, "bob", &verifier)
                        .unwrap();
                }
                return;
            }
        }
    }
    panic!("global password change limit should have triggered");
}

#[test]
fn account_login_isolates_unknown_usernames_and_hides_block_status_until_verified() {
    let mut authority = fixture();
    let admin_verifier = test_verifier("admin password");
    let admin = authority
        .initialize_admin("init", "init", "admin", &admin_verifier)
        .unwrap();
    let verifier = test_verifier("correct horse battery");
    let wrong = test_verifier("wrong password");
    let (alice_id, _) = seed_password_account(&mut authority, 1, "alice", &verifier);

    // Unknown usernames deny immediately without creating per-account or global throttle rows.
    for i in 0..10 {
        assert!(matches!(
            fault_of(authority.login("probe-ip", &format!("missing{i}"), &wrong)),
            Fault::Denied { reason } if reason == "credentials"
        ));
    }
    assert_eq!(
        authority
            .store
            .scan("throttle/account-login-account/", None)
            .unwrap()
            .len(),
        0
    );
    assert_eq!(
        authority
            .store
            .scan("throttle/account-login-global/", None)
            .unwrap()
            .len(),
        0
    );
    assert!(matches!(
        fault_of(authority.login("probe-ip", "alice", &verifier)),
        Fault::RateLimited { retry_after: 60 }
    ));

    // Blocking alice stays hidden on wrong password and is revealed only after the verifier matches.
    authority
        .block(
            &admin,
            Block::Account {
                id: alice_id,
                blocked: true,
            },
        )
        .unwrap();
    assert!(matches!(
        fault_of(authority.login("blocked-probe-1", "alice", &wrong)),
        Fault::Denied { reason } if reason == "credentials"
    ));
    assert!(matches!(
        fault_of(authority.login("blocked-probe-2", "alice", &verifier)),
        Fault::Denied { reason } if reason == "account_blocked"
    ));
}

#[test]
fn account_login_enforces_account_and_global_buckets_atomically() {
    let mut authority = fixture();
    let verifier = test_verifier("correct horse battery");
    seed_password_account(&mut authority, 1, "alice", &verifier);
    seed_password_account(&mut authority, 2, "bob", &verifier);

    // Distributed IPs targeting alice hit the per-account limit after 10 attempts.
    for i in 0..10 {
        authority
            .login(&format!("dist-a-{i}"), "alice", &verifier)
            .unwrap();
    }
    assert!(matches!(
        fault_of(authority.login("dist-a-extra", "alice", &verifier)),
        Fault::RateLimited { retry_after: 60 }
    ));

    // Exhaust the remaining global login burst (120 total; 10 spent above).
    let mut spent = 10;
    for acct_idx in 3..=16u8 {
        let name = format!("member{acct_idx}");
        seed_password_account(&mut authority, acct_idx, &name, &verifier);
        for attempt in 0..10 {
            let source = format!("login-global-{acct_idx}-{attempt}");
            let result = authority.login(&source, &name, &verifier);
            if spent < 120 {
                result.unwrap();
                spent += 1;
            } else {
                // Global exhaustion must not drain bob's untouched source or account bucket.
                assert!(matches!(
                    fault_of(authority.login("bob-ip", "bob", &verifier)),
                    Fault::RateLimited { retry_after: 1 }
                ));
                assert!(matches!(
                    fault_of(result),
                    Fault::RateLimited { retry_after: 1 }
                ));
                for _ in 0..10 {
                    authority.environment.now += 1;
                    authority.login("bob-ip", "bob", &verifier).unwrap();
                }
                return;
            }
        }
    }
    panic!("global account login limit should have triggered");
}

#[test]
fn admin_login_isolates_unknown_usernames_and_enforces_limits() {
    let mut authority = fixture();
    let verifier = test_verifier("correct horse battery");
    let wrong = test_verifier("wrong admin password");
    let admin_session = authority
        .initialize_admin("init", "init", "admin", &verifier)
        .unwrap();
    authority.require_admin(&admin_session).unwrap();

    // Wrong admin username spends only source budget, never the global admin bucket.
    for _ in 0..10 {
        assert!(matches!(
            fault_of(authority.admin_login("wrong-user-ip", "not-admin", &verifier)),
            Fault::Denied { reason } if reason == "credentials"
        ));
    }
    assert_eq!(
        authority
            .store
            .scan("throttle/admin-login-global/", None)
            .unwrap()
            .len(),
        0
    );
    assert!(matches!(
        fault_of(authority.admin_login("wrong-user-ip", "admin", &verifier)),
        Fault::RateLimited { retry_after: 60 }
    ));

    // Wrong admin verifier is denied while consuming attempt capacity.
    assert!(matches!(
        fault_of(authority.admin_login("operator-ip", "admin", &wrong)),
        Fault::Denied { reason } if reason == "credentials"
    ));

    // Successful admin login issues a valid session.
    let logged_in = authority
        .admin_login("operator-ip", "admin", &verifier)
        .unwrap();
    authority.require_admin(&logged_in).unwrap();

    // Exhaust source bucket (8 remaining after the 2 attempts above).
    for _ in 0..8 {
        authority
            .admin_login("operator-ip", "admin", &verifier)
            .unwrap();
    }
    assert!(matches!(
        fault_of(authority.admin_login("operator-ip", "admin", &verifier)),
        Fault::RateLimited { retry_after: 60 }
    ));

    // Exhaust remaining global admin login burst (30 total; 10 spent above).
    for i in 0..20 {
        authority
            .admin_login(&format!("bot-{i}"), "admin", &verifier)
            .unwrap();
    }
    // Global exhaustion must not drain a fresh operator IP's source bucket.
    assert!(matches!(
        fault_of(authority.admin_login("fresh-op", "admin", &verifier)),
        Fault::RateLimited { retry_after: 2 }
    ));
    for _ in 0..10 {
        authority.environment.now += 2;
        authority
            .admin_login("fresh-op", "admin", &verifier)
            .unwrap();
    }
}

#[test]
fn registration_rejects_credential_fields_and_consumes_pow_proof_atomically() {
    let mut authority = fixture();
    let settings = Settings {
        registration: RegistrationPolicy::ProofOfWork,
        pow_bits: 4,
        ..Settings::default()
    };
    save(&mut authority.store, "settings", &settings).unwrap();

    let mut request = registration(4);
    let secret = request.client_secret.clone();
    let verifier = test_verifier("correct horse battery");
    let challenge = match fault_of(authority.register("src", request.clone())) {
        Fault::Verification { challenge } => challenge,
        other => panic!("expected work challenge, got {other:?}"),
    };
    let solution = (0..4096)
        .map(|nonce| nonce.to_string())
        .find(|nonce| {
            let digest = Sha256::new()
                .chain_update(challenge.id.as_bytes())
                .chain_update(nonce.as_bytes())
                .finalize();
            digest[0].leading_zeros() >= u32::from(challenge.bits)
        })
        .unwrap();
    request.challenge = Some(challenge.id);
    request.solution = Some(solution);

    let created = authority.register("src", request.clone()).unwrap();
    let retried = authority.register("src", request).unwrap();
    assert_eq!(retried.account, created.account);
    assert_eq!(retried.credential, created.credential);
    let named = authority
        .set_password("src", &secret, "carol", &verifier)
        .unwrap();
    assert_eq!(named.username.as_deref(), Some("carol"));

    // Registration rejects credential fields at deserialization so POST /v1/accounts
    // cannot be abused as an unauthenticated username enumeration oracle.
    let probe = serde_json::json!({
        "client_secret": registration(5).client_secret,
        "username": "carol",
        "password_verifier": String::from(verifier),
    });
    assert!(serde_json::from_value::<Registration>(probe).is_err());
}
