use super::*;
use crate::public_blocks::PublicBlockFeed;
use sha2::{Digest, Sha256};

#[test]
fn account_contract_vectors_match_go_and_browser() {
    let mut authority = fixture();
    let session = authority.register("vector", registration(90)).unwrap();
    authority
        .set_nickname(&registration(90).client_secret, "春风")
        .unwrap();
    let refreshed = authority.refresh(&registration(90).client_secret).unwrap();
    let root = SigningKey::from_bytes(&[41; 32]);
    let node = SigningKey::from_bytes(&[43; 32]);
    let generation = Generation {
        number: 2,
        ..authority.signing.generation
    };
    let second_certificate = URL_SAFE_NO_PAD.encode(generation.certify(&root).unwrap());
    let mut trusted = TrustStore::default();
    trusted
        .accept(authority.signing.generation, authority.environment.now())
        .unwrap();
    let ticket = Ticket::verify(
        &URL_SAFE_NO_PAD.decode(&session.ticket).unwrap(),
        &trusted,
        authority.environment.now(),
    )
    .unwrap();
    let mut noncanonical = ticket.encode();
    noncanonical.splice(1..2, [24, 1]);
    let noncanonical = URL_SAFE_NO_PAD.encode(trust::sign(
        b"windshare/account-ticket/v1\0",
        &noncanonical,
        &authority.signing.key,
    ));
    let descriptor = serde_json::to_vec(&serde_json::json!({
        "version":2,"node_key":URL_SAFE_NO_PAD.encode(node.verifying_key().to_bytes()),
        "node_id":URL_SAFE_NO_PAD.encode(Sha256::new().chain_update(b"windshare/node-identity/v1\0").chain_update(node.verifying_key().to_bytes()).finalize()),
        "relay_endpoint":"wss://node.example/v2/ws","services":{"relay":true,"stun":false},
        "stun":[],"zone":"global","region":"test","capacity_bytes_per_second":1_000_000,
        "classes":["owner","standard","lowest"],"issued_at":authority.environment.now(),
        "expires_at":authority.environment.now()+300,"relay_available":true,"anonymous":true
    })).unwrap();
    let signed = trust::sign(b"windshare/node-descriptor/v2\0", &descriptor, &node);
    let mut feed = PublicBlockFeed::default();
    let blocks: serde_json::Value =
        serde_json::from_str(&feed.snapshot(&authority).unwrap().body).unwrap();
    let data = serde_json::to_vec_pretty(&serde_json::json!({
        "now":authority.environment.now(),
        "root":URL_SAFE_NO_PAD.encode(authority.signing.generation.issuer),
        "session":session,"refreshed":refreshed,
        "next_certificate":second_certificate,
        "noncanonical_ticket":noncanonical,
        "node_key":URL_SAFE_NO_PAD.encode(node.verifying_key().to_bytes()),
        "descriptor":{"version":2,"descriptor":URL_SAFE_NO_PAD.encode(&descriptor),"signature":URL_SAFE_NO_PAD.encode(&signed[descriptor.len()..])},
        "blocks":blocks
    })).unwrap();
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../../testdata/network/authority.json");
    if std::env::var("UPDATE_AUTHORITY_VECTORS").as_deref() == Ok("1") {
        std::fs::write(&path, &data).unwrap();
    }
    assert_eq!(
        std::fs::read(path).unwrap(),
        data,
        "refresh with UPDATE_AUTHORITY_VECTORS=1"
    );
}

#[test]
fn email_requires_delivered_secret_and_password_setup_keeps_account_identity() {
    let mut authority = fixture();
    let mut settings = authority.settings().unwrap();
    settings.registration = RegistrationPolicy::Email;
    save(&mut authority.store, "settings", &settings).unwrap();
    let request = registration(7);
    assert!(authority.register("source", request.clone()).is_err());
    let (challenge, secret) = authority
        .deliver_email(
            "source",
            registration_email(&request),
            &Mailbox::parse("alice@qq.com").unwrap(),
        )
        .unwrap();
    let mut proof = request.clone();
    proof.challenge = Some(challenge.id);
    proof.solution = Some("anything".into());
    assert!(authority.register("source", proof.clone()).is_err());
    proof.solution = Some(secret);
    let session = authority.register("source", proof).unwrap();
    assert_eq!(session.profile.email.as_deref(), Some("alice@qq.com"));
    let updated = authority
        .set_password(&request.client_secret, "Alice", "correct horse battery")
        .unwrap();
    assert_eq!(updated.account, session.account);
    let sibling = authority.login("alice", "correct horse battery").unwrap();
    assert_eq!(sibling.account, session.account);
    assert_ne!(sibling.credential, session.credential);
}

#[test]
fn mistyped_registration_email_code_keeps_its_challenge_until_attempts_run_out() {
    let mut authority = fixture();
    let mut settings = authority.settings().unwrap();
    settings.registration = RegistrationPolicy::Email;
    save(&mut authority.store, "settings", &settings).unwrap();
    let address = Mailbox::parse("alice@qq.com").unwrap();
    let mut proof = registration(8);
    let (delivered, code) = authority
        .deliver_email("source", registration_email(&proof), &address)
        .unwrap();
    proof.challenge = Some(delivered.id.clone());
    proof.solution = Some("mistyped".into());
    let prompt = |authority: &mut Authority<Memory, Clock>, proof: &Registration| match authority
        .register("source", proof.clone())
        .unwrap_err()
        .downcast::<Fault>()
        .unwrap()
    {
        Fault::Verification { challenge } => challenge,
        other => panic!("expected a verification prompt, got {other:?}"),
    };
    let same = |left: &Challenge, right: &Challenge| {
        (&left.id, left.mode, left.bits, left.expires_at)
            == (&right.id, right.mode, right.bits, right.expires_at)
    };
    // A typo answers with the delivered challenge itself (same id, mode and original
    // expiry), so every client keeps it. Each attempt still counts and persists.
    for _ in 0..4 {
        authority.environment.now += 10;
        assert!(same(&prompt(&mut authority, &proof), &delivered));
    }
    // The 5th wrong code uses the challenge up: back to the id-less email prompt.
    let used = prompt(&mut authority, &proof);
    assert!(used.id.is_empty() && used.mode == RegistrationMode::Email);
    proof.solution = Some(code);
    assert_eq!(prompt(&mut authority, &proof).id, "");

    // Unknown ids and expired challenges keep the id-less prompt too.
    let (delivered, code) = authority
        .deliver_email("source", registration_email(&proof), &address)
        .unwrap();
    proof.challenge = Some("unknown".into());
    proof.solution = Some("mistyped".into());
    assert_eq!(prompt(&mut authority, &proof).id, "");
    proof.challenge = Some(delivered.id.clone());
    assert!(same(&prompt(&mut authority, &proof), &delivered));
    proof.solution = Some(code);
    let session = authority.register("source", proof.clone()).unwrap();
    assert_eq!(session.profile.email.as_deref(), Some("alice@qq.com"));

    proof.client_secret = registration(9).client_secret;
    let (delivered, _) = authority
        .deliver_email("other-source", registration_email(&proof), &address)
        .unwrap();
    proof.challenge = Some(delivered.id.clone());
    proof.solution = Some("mistyped".into());
    authority.environment.now = delivered.expires_at;
    let expired = authority
        .register("other-source", proof)
        .unwrap_err()
        .downcast::<Fault>()
        .unwrap();
    assert!(matches!(expired, Fault::Verification { challenge } if challenge.id.is_empty()));
}

#[test]
fn released_clients_redeem_registration_email_by_challenge() {
    let mut authority = fixture();
    let mut settings = authority.settings().unwrap();
    settings.registration = RegistrationPolicy::Email;
    save(&mut authority.store, "settings", &settings).unwrap();
    let address = Mailbox::parse("carol@qq.com").unwrap();
    // v0.1.3 requests the code without its setup secret.
    let released = EmailVerification::Registration {
        client_secret: None,
    };
    let (delivered, code) = authority
        .deliver_email("source", released, &address)
        .unwrap();
    let mut proof = registration(10);
    proof.challenge = Some(delivered.id.clone());
    proof.solution = Some("mistyped".into());
    match authority
        .register("source", proof.clone())
        .unwrap_err()
        .downcast::<Fault>()
        .unwrap()
    {
        Fault::Verification { challenge } => assert_eq!(challenge.id, delivered.id),
        other => panic!("expected a verification prompt, got {other:?}"),
    }
    // A challenge for one setup secret is not reachable through the released binding.
    let (bound, bound_code) = authority
        .deliver_email(
            "other-source",
            registration_email(&registration(11)),
            &address,
        )
        .unwrap();
    let mut other = proof.clone();
    other.challenge = Some(bound.id);
    other.solution = Some(bound_code);
    assert!(authority.register("other-source", other).is_err());
    proof.solution = Some(code);
    let session = authority.register("source", proof).unwrap();
    assert_eq!(session.profile.email.as_deref(), Some("carol@qq.com"));
}

#[test]
fn the_last_work_registration_reuses_its_challenge_and_creates_a_profile_without_email() {
    let mut authority = fixture();
    let settings = Settings {
        pow_bits: 4,
        ..Settings::default()
    };
    save(&mut authority.store, "settings", &settings).unwrap();
    save(&mut authority.store, "account-count", &3_999u64).unwrap();
    authority.environment.registration_email_configured = true;
    let mut request = registration(9);
    let pending = |error: anyhow::Error| match error.downcast::<Fault>().unwrap() {
        Fault::Verification { challenge } => challenge,
        other => panic!("expected a work challenge, got {other}"),
    };
    let challenge = pending(authority.register("source", request.clone()).unwrap_err());
    let retried = pending(authority.register("source", request.clone()).unwrap_err());
    assert_eq!(challenge.id, retried.id);
    assert_eq!(challenge.mode, RegistrationMode::ProofOfWork);
    let solution = (0..4096)
        .map(|nonce| nonce.to_string())
        .find(|nonce| {
            let hash = Sha256::new()
                .chain_update(challenge.id.as_bytes())
                .chain_update(nonce.as_bytes())
                .finalize();
            hash[0].leading_zeros() >= u32::from(challenge.bits)
        })
        .unwrap();
    request.challenge = Some(challenge.id);
    request.solution = Some(solution);
    let registered = authority.register("source", request).unwrap();
    assert_eq!(registered.profile, Profile::default());
    assert_eq!(authority.account_count().unwrap(), 4_000);
}
