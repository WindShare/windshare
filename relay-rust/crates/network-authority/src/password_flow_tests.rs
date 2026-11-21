use super::*;
use argon2::{
    Argon2,
    password_hash::{PasswordHasher, SaltString},
};
use sha2::{Digest, Sha256};
use windshare_relay_access::{AccountId, device_login::Invitation};

fn fault_of<T: std::fmt::Debug>(result: Result<T>) -> Fault {
    result.unwrap_err().downcast::<Fault>().unwrap()
}

fn seed_password_account(
    authority: &mut Authority<Memory, Clock>,
    secret_byte: u8,
    username: &str,
    hash: &HashedPassword,
) -> (AccountId, String) {
    let mut request = registration(secret_byte);
    let secret = request.client_secret.clone();
    request.username = Some(username.into());
    request.password = Some(hash.clone());
    let session = authority
        .register(&format!("seed-{secret_byte}"), request)
        .unwrap();
    (session.account, secret)
}

fn fast_hash_fixture() -> HashedPassword {
    HashedPassword::hash("correct horse battery", &[7; PASSWORD_SALT_BYTES]).unwrap()
}

#[test]
fn independent_random_salts_prevent_cookie_resurrection_and_verify_legacy_hashes() {
    let mut authority = fixture();
    let original = "correct horse battery";
    let replacement = "another valid password";
    let secret = registration(1).client_secret;
    let session = authority
        .register_with_password("source", registration(1), "alice", original)
        .unwrap();
    let initial_account = load::<Account>(&authority.store, &key("account", &session.account))
        .unwrap()
        .unwrap();
    let initial_hash = initial_account.password.unwrap();
    let account_id_salt = SaltString::encode_b64(&session.account).unwrap();
    assert!(
        !initial_hash.as_str().contains(account_id_salt.as_str()),
        "password salt must not reuse the public account ID"
    );

    let signer = SigningKey::from_bytes(&[9; 32]);
    let node = signer.verifying_key().to_bytes();
    save(
        &mut authority.store,
        &key("node", &node),
        &Node {
            listing: None,
            key: node,
            zone: Zone::Global,
            region: "test".into(),
            enabled: true,
            anonymous: false,
            version: 1,
        },
    )
    .unwrap();
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
        .device_sign_in("source", &invite, "alice", original)
        .unwrap()
        .session;
    assert!(authority.browser_account(&cookie).is_some());

    // Changing A -> B -> A must produce a fresh salt and keep the old cookie invalid.
    authority
        .set_password("source", &secret, "alice", replacement)
        .unwrap();
    authority
        .set_password("source", &secret, "alice", original)
        .unwrap();
    let final_hash = load::<Account>(&authority.store, &key("account", &session.account))
        .unwrap()
        .unwrap()
        .password
        .unwrap();
    assert_ne!(initial_hash, final_hash);
    assert!(final_hash.verify(original));
    assert!(authority.browser_account(&cookie).is_none());

    // Previously persisted Argon2::default() (19 MiB, t=2) hashes remain accepted.
    let legacy_salt = SaltString::encode_b64(&[11; PASSWORD_SALT_BYTES]).unwrap();
    let legacy_raw = Argon2::default()
        .hash_password(original.as_bytes(), &legacy_salt)
        .unwrap()
        .to_string();
    let legacy = HashedPassword::try_from(legacy_raw).unwrap();
    assert!(legacy.verify(original));
    assert!(!legacy.verify(replacement));
    assert!(HashedPassword::try_from(String::from("plaintext-password")).is_err());
    assert!(HashedPassword::try_from(String::new()).is_err());
}

#[test]
fn password_change_rejects_stale_concurrent_updates() {
    let mut authority = fixture();
    let hash = fast_hash_fixture();
    let (_, secret_a) = seed_password_account(&mut authority, 1, "alice", &hash);

    let first = authority
        .prepare_password_change("ip-cas-1", &secret_a, "alice", 16)
        .unwrap();
    let second = authority
        .prepare_password_change("ip-cas-2", &secret_a, "alice", 16)
        .unwrap();
    let next_hash =
        HashedPassword::hash("second valid password", &[8; PASSWORD_SALT_BYTES]).unwrap();
    authority
        .commit_password_change(
            &secret_a,
            "alice",
            next_hash.clone(),
            first.previous_verifier_digest,
        )
        .unwrap();
    assert!(matches!(
        fault_of(authority.commit_password_change(
            &secret_a,
            "alice",
            next_hash,
            second.previous_verifier_digest,
        )),
        Fault::Denied { reason } if reason == "credentials_changed"
    ));
}

#[test]
fn password_change_limits_atomically_without_burning_global_on_invalid_input() {
    let mut authority = fixture();
    let hash = fast_hash_fixture();
    let (_, secret_a) = seed_password_account(&mut authority, 1, "alice", &hash);
    let (_, secret_b) = seed_password_account(&mut authority, 2, "bob", &hash);

    // Invalid inputs and taken usernames do not spend account or global quotas.
    for _ in 0..10 {
        assert!(matches!(
            fault_of(authority.prepare_password_change("typo-ip", &secret_a, "alice", 5)),
            Fault::Invalid
        ));
        assert!(matches!(
            fault_of(authority.prepare_password_change("typo-ip", &secret_a, "bob", 16)),
            Fault::Denied { reason } if reason == "username_taken"
        ));
    }
    assert_eq!(
        authority
            .store
            .scan("throttle/account-password-global/", None)
            .unwrap()
            .len(),
        0
    );

    // Invalid credentials count against only the caller's source bucket.
    for _ in 0..5 {
        assert!(matches!(
            fault_of(authority.prepare_password_change("bad-ip", "wrong-secret", "alice", 16)),
            Fault::Denied { reason } if reason == "credentials"
        ));
    }
    assert!(matches!(
        fault_of(authority.prepare_password_change("bad-ip", &secret_a, "alice", 16)),
        Fault::RateLimited { retry_after: 60 }
    ));

    // Rotating source IPs hits the per-account bucket after 5 attempts.
    for i in 0..5 {
        authority
            .prepare_password_change(&format!("ip-a-{i}"), &secret_a, "alice", 16)
            .unwrap();
    }
    assert!(matches!(
        fault_of(authority.prepare_password_change("ip-a-extra", &secret_a, "alice", 16)),
        Fault::RateLimited { retry_after: 60 }
    ));

    // Exhaust the remaining global password-change burst (60 total; 5 spent above).
    let mut spent = 5;
    for acct_idx in 3..=20u8 {
        let (_, secret) =
            seed_password_account(&mut authority, acct_idx, &format!("user{acct_idx}"), &hash);
        for attempt in 0..5 {
            let source = format!("global-{acct_idx}-{attempt}");
            let result =
                authority.prepare_password_change(&source, &secret, &format!("user{acct_idx}"), 16);
            if spent < 60 {
                result.unwrap();
                spent += 1;
            } else {
                // Global exhaustion must not drain bob's untouched source or account bucket.
                assert!(matches!(
                    fault_of(authority.prepare_password_change(
                        "innocent-ip",
                        &secret_b,
                        "bob",
                        16
                    )),
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
                        .prepare_password_change("innocent-ip", &secret_b, "bob", 16)
                        .unwrap();
                }
                return;
            }
        }
    }
    panic!("global password change limit should have triggered");
}

#[test]
fn account_login_throttles_atomically_and_hides_block_status_until_verified() {
    let mut authority = fixture();
    let admin = authority
        .complete_initialization("init", "init", "admin", fast_hash_fixture())
        .unwrap();
    let hash = fast_hash_fixture();
    let (alice_id, _) = seed_password_account(&mut authority, 1, "alice", &hash);
    let (_, _) = seed_password_account(&mut authority, 2, "bob", &hash);

    // Unknown usernames deny immediately without creating per-account or global throttle rows.
    for i in 0..10 {
        assert!(matches!(
            fault_of(authority.begin_account_login("probe-ip", &format!("missing{i}"))),
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
        fault_of(authority.begin_account_login("probe-ip", "alice")),
        Fault::RateLimited { retry_after: 60 }
    ));

    // Blocking alice must not be leaked during begin_account_login before password verification.
    authority
        .block(
            &admin,
            Block::Account {
                id: alice_id,
                blocked: true,
            },
        )
        .unwrap();
    let blocked_challenge = authority
        .begin_account_login("blocked-probe", "alice")
        .unwrap();
    assert!(matches!(
        fault_of(authority.complete_account_login(
            blocked_challenge.account,
            blocked_challenge.password.fingerprint(),
        )),
        Fault::Denied { reason } if reason == "account_blocked"
    ));
    authority
        .block(
            &admin,
            Block::Account {
                id: alice_id,
                blocked: false,
            },
        )
        .unwrap();

    // Distributed IPs targeting alice hit the per-account limit after 10 total attempts (1 above + 9 below).
    for i in 0..9 {
        authority
            .begin_account_login(&format!("dist-a-{i}"), "alice")
            .unwrap();
    }
    assert!(matches!(
        fault_of(authority.begin_account_login("dist-a-extra", "alice")),
        Fault::RateLimited { retry_after: 60 }
    ));

    // Exhaust the remaining global login burst (120 total; 10 spent above).
    let mut spent = 10;
    for acct_idx in 3..=16u8 {
        let name = format!("member{acct_idx}");
        seed_password_account(&mut authority, acct_idx, &name, &hash);
        for attempt in 0..10 {
            let source = format!("login-global-{acct_idx}-{attempt}");
            let result = authority.begin_account_login(&source, &name);
            if spent < 120 {
                result.unwrap();
                spent += 1;
            } else {
                // Global exhaustion must not drain bob's untouched source or account bucket.
                assert!(matches!(
                    fault_of(authority.begin_account_login("bob-ip", "bob")),
                    Fault::RateLimited { retry_after: 1 }
                ));
                assert!(matches!(
                    fault_of(result),
                    Fault::RateLimited { retry_after: 1 }
                ));
                for _ in 0..10 {
                    authority.environment.now += 1;
                    authority.begin_account_login("bob-ip", "bob").unwrap();
                }
                return;
            }
        }
    }
    panic!("global account login limit should have triggered");
}

#[test]
fn admin_login_isolates_unknown_usernames_enforces_limits_and_rejects_stale_verifiers() {
    let mut authority = fixture();
    let hash = fast_hash_fixture();
    let admin_session = authority
        .complete_initialization("init", "init", "admin", hash)
        .unwrap();
    authority.require_admin(&admin_session).unwrap();

    // Wrong admin username spends only source budget, never the global admin bucket.
    for _ in 0..10 {
        assert!(matches!(
            fault_of(authority.begin_admin_login("wrong-user-ip", "not-admin")),
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
        fault_of(authority.begin_admin_login("wrong-user-ip", "admin")),
        Fault::RateLimited { retry_after: 60 }
    ));

    // Wrong admin password digest is refused in Phase 2.
    assert!(matches!(
        fault_of(authority.complete_admin_login([0; 32])),
        Fault::Denied { reason } if reason == "credentials"
    ));

    // Successful two-phase admin login works through the helper.
    let logged_in = authority
        .admin_login("operator-ip", "admin", "correct horse battery")
        .unwrap();
    authority.require_admin(&logged_in).unwrap();

    // Exhaust source bucket (9 remaining after the 1 login above).
    for _ in 0..9 {
        authority.begin_admin_login("operator-ip", "admin").unwrap();
    }
    assert!(matches!(
        fault_of(authority.begin_admin_login("operator-ip", "admin")),
        Fault::RateLimited { retry_after: 60 }
    ));

    // Exhaust remaining global admin login burst (30 total; 10 spent above).
    for i in 0..20 {
        authority
            .begin_admin_login(&format!("bot-{i}"), "admin")
            .unwrap();
    }
    // Global exhaustion must not drain a fresh operator IP's source bucket.
    assert!(matches!(
        fault_of(authority.begin_admin_login("fresh-op", "admin")),
        Fault::RateLimited { retry_after: 2 }
    ));
    for _ in 0..10 {
        authority.environment.now += 2;
        authority.begin_admin_login("fresh-op", "admin").unwrap();
    }
}

#[test]
fn login_completion_rejects_password_changes_and_account_blocks_after_edge_verify() {
    let mut authority = fixture();
    let admin = authority
        .complete_initialization("init", "init", "admin", fast_hash_fixture())
        .unwrap();
    let hash_a = fast_hash_fixture();
    let hash_b = HashedPassword::hash("second valid password", &[8; PASSWORD_SALT_BYTES]).unwrap();
    let (account, secret) = seed_password_account(&mut authority, 1, "alice", &hash_a);

    let challenge = authority.begin_account_login("ip-1", "alice").unwrap();
    authority
        .commit_password_change(&secret, "alice", hash_b, Some(hash_a.fingerprint()))
        .unwrap();
    assert!(matches!(
        fault_of(authority.complete_account_login(challenge.account, challenge.password.fingerprint())),
        Fault::Denied { reason } if reason == "credentials"
    ));

    let fresh = authority.begin_account_login("ip-2", "alice").unwrap();
    authority
        .block(
            &admin,
            Block::Account {
                id: account,
                blocked: true,
            },
        )
        .unwrap();
    assert!(matches!(
        fault_of(authority.complete_account_login(fresh.account, fresh.password.fingerprint())),
        Fault::Denied { reason } if reason == "account_blocked"
    ));
}

#[test]
fn password_registration_prepare_keeps_valid_proof_for_commit_and_handles_retries() {
    let mut authority = fixture();
    let settings = Settings {
        registration: RegistrationPolicy::ProofOfWork,
        pow_bits: 4,
        ..Settings::default()
    };
    save(&mut authority.store, "settings", &settings).unwrap();

    let base = registration(4);
    let mut prepare = PasswordRegistrationPrepare {
        client_secret: base.client_secret.clone(),
        challenge: None,
        solution: None,
        username: "carol".into(),
        password_len: 21,
    };
    let challenge = match fault_of(authority.prepare_password_registration("src", &prepare)) {
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
    prepare.challenge = Some(challenge.id.clone());
    prepare.solution = Some(solution.clone());

    assert!(matches!(
        authority
            .prepare_password_registration("src", &prepare)
            .unwrap(),
        PasswordRegistrationPreparation::Ready
    ));

    // Commit consumes the preserved proof atomically without re-hashing inside DO.
    let created = authority
        .register(
            "src",
            Registration {
                client_secret: base.client_secret,
                challenge: Some(challenge.id),
                solution: Some(solution),
                username: Some("carol".into()),
                password: Some(fast_hash_fixture()),
            },
        )
        .unwrap();

    // Idempotent retry short-circuits with Existing without running edge Argon2.
    match authority
        .prepare_password_registration("src", &prepare)
        .unwrap()
    {
        PasswordRegistrationPreparation::Existing { session } => {
            assert_eq!(session.account, created.account);
            assert_eq!(session.credential, created.credential);
        }
        PasswordRegistrationPreparation::Ready => panic!("expected idempotent Existing session"),
    }

    // Mismatched username/password pair in direct register is rejected.
    let mut mismatched = registration(5);
    mismatched.username = Some("orphan".into());
    assert!(matches!(
        fault_of(authority.register("src", mismatched)),
        Fault::Invalid
    ));
}
