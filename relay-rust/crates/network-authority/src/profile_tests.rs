use super::*;

#[test]
fn nickname_deadline_follows_the_account_across_logins_and_restarts() {
    let mut authority = fixture();
    let secret = registration(1).client_secret;
    let first = authority.register("source", registration(1)).unwrap();
    for invalid in ["   ", "line\nbreak", &"春".repeat(33)] {
        assert!(matches!(
            authority
                .set_nickname(&secret, invalid)
                .unwrap_err()
                .downcast_ref::<Fault>(),
            Some(Fault::InvalidNickname)
        ));
    }
    let profile = authority.set_nickname(&secret, "  春风 🌱  ").unwrap();
    let nickname = profile.nickname.as_ref().unwrap();
    assert_eq!(nickname.value, "春风 🌱");
    assert_eq!(
        nickname.next_change_at,
        authority.environment.now + 30 * DAY_SECONDS
    );
    authority.environment.now += 1;
    assert_eq!(authority.set_nickname(&secret, "春风 🌱").unwrap(), profile);
    let verifier = test_verifier("correct horse battery");
    let login = authority
        .set_password("source", &secret, "alice", &verifier)
        .unwrap();
    assert_eq!(login.profile, profile);
    let sibling = authority.login("source", "alice", &verifier).unwrap();
    assert_eq!(first.account, sibling.account);
    let sibling_secret = sibling.credential_secret.unwrap();
    authority = Authority::new(authority.store, authority.environment, authority.signing).unwrap();
    authority.environment.now = nickname.next_change_at - 1;
    assert!(
        matches!(authority.set_nickname(&sibling_secret, "新名字").unwrap_err().downcast_ref::<Fault>(),
        Some(Fault::NicknameCooldown { profile: current }) if current == &profile)
    );
    authority.environment.now += 1;
    let next = authority.set_nickname(&sibling_secret, "新名字").unwrap();
    assert_eq!(
        next.nickname.unwrap().next_change_at,
        nickname.next_change_at + 30 * DAY_SECONDS
    );
    assert_eq!(next.revision, profile.revision + 1);
}

#[test]
fn provider_policy_matches_the_whole_domain_and_canonicalizes_aliases() {
    for address in [
        "one@qq.com",
        "vip@vip.qq.com",
        "first.last+tag@163.com",
        "Test@GMAIL.COM",
        "me@outlook.com",
        "me@live.cn",
        "me@icloud.com",
        "me@fastmail.com",
    ] {
        assert!(Mailbox::parse(address).is_ok(), "{address}");
    }
    let parsed = Mailbox::parse("  Alice+tag@QQ.COM ").unwrap();
    assert_eq!(parsed.as_str(), "Alice+tag@qq.com");
    assert_eq!(parsed.canonical(), "alice@qq.com");

    let gmail = Mailbox::parse("A.l.i.c.e+wind@GOOGLEMAIL.COM").unwrap();
    assert_eq!(gmail.as_str(), "A.l.i.c.e+wind@googlemail.com");
    assert_eq!(gmail.canonical(), "alice@gmail.com");
    assert_eq!(
        Mailbox::parse("alice@gmail.com").unwrap().canonical(),
        gmail.canonical()
    );
    assert_eq!(
        Mailbox::parse("first.last+tag@163.com")
            .unwrap()
            .canonical(),
        "first.last@163.com"
    );

    let extra = Mailbox::parse_extra_domains(
        " Example.EDU , bad_domain , -bad.org , 127.0.0.1 , sub.corp.cn ",
    );
    assert_eq!(extra, vec!["example.edu", "sub.corp.cn"]);
    assert_eq!(
        Mailbox::parse_with_extra_domains("User+lab@EXAMPLE.EDU", &extra)
            .unwrap()
            .canonical(),
        "user@example.edu"
    );
    assert!(matches!(
        Mailbox::parse_with_extra_domains("user@other.edu", &extra),
        Err(Fault::UnsupportedEmail)
    ));

    for address in [
        "a@example.com",
        "a@sub.qq.com",
        "a@qq.com.example",
        "a@qԛ.com",
        "a@nodot",
        "a@-qq.com",
        "a@qq-.com",
        "@qq.com",
        "a@@qq.com",
        ".a@gmail.com",
        "a..b@gmail.com",
        "a b@gmail.com",
        "a\nb@qq.com",
    ] {
        assert!(Mailbox::parse(address).is_err(), "{address}");
    }
}

#[test]
fn only_the_current_code_for_the_target_account_and_new_mailbox_can_replace_email() {
    let mut authority = fixture();
    let first = registration(1).client_secret;
    let other = registration(2).client_secret;
    authority.register("one", registration(1)).unwrap();
    authority.register("two", registration(2)).unwrap();
    let original = || Mailbox::parse("alice@qq.com").unwrap();
    let next = || Mailbox::parse("alice@163.com").unwrap();
    let send = |authority: &mut Authority<Memory, Clock>, source: &str, email: Mailbox| {
        authority
            .deliver_email(source, profile_email(&first), &email)
            .unwrap()
    };
    let code = send(&mut authority, "one", original());
    assert!(
        authority
            .confirm_email(&other, original(), &code.0.id, &code.1)
            .is_err()
    );
    assert!(
        authority
            .confirm_email(&first, next(), &code.0.id, &code.1)
            .is_err()
    );
    let verified = authority
        .confirm_email(&first, original(), &code.0.id, &code.1)
        .unwrap();
    assert_eq!(verified.email.as_deref(), Some("alice@qq.com"));
    assert_eq!(
        authority
            .confirm_email(&first, original(), &code.0.id, &code.1)
            .unwrap(),
        verified
    );
    let stale = send(&mut authority, "one", next());
    let fresh = send(&mut authority, "one", next());
    assert!(
        authority
            .confirm_email(&first, next(), &stale.0.id, &stale.1)
            .is_err()
    );
    assert_eq!(authority.refresh(&first).unwrap().profile, verified);
    let changed = authority
        .confirm_email(&first, next(), &fresh.0.id, &fresh.1)
        .unwrap();
    assert_eq!(changed.email.as_deref(), Some("alice@163.com"));
    assert_eq!(changed.revision, verified.revision + 1);
    assert!(
        authority
            .confirm_email(&first, original(), &code.0.id, &code.1)
            .is_err()
    );
    assert!(authority.refresh(&other).unwrap().profile.email.is_none());
}

#[test]
fn registration_codes_cannot_edit_profiles_and_expired_or_exhausted_codes_cannot_be_redeemed() {
    let mut authority = fixture();
    let secret = registration(1).client_secret;
    authority.register("one", registration(1)).unwrap();
    let address = || Mailbox::parse("alice@gmail.com").unwrap();
    // Registration mail is refused until the active registration mode requires email.
    assert!(matches!(
        authority
            .deliver_email("one", registration_email(&registration(2)), &address())
            .unwrap_err()
            .downcast_ref::<Fault>(),
        Some(Fault::Denied { reason }) if reason == "email_registration_disabled"
    ));
    let mut settings = authority.settings().unwrap();
    settings.registration = RegistrationPolicy::Email;
    save(&mut authority.store, "settings", &settings).unwrap();
    // An already-registered setup secret cannot request registration mail.
    assert!(matches!(
        authority
            .deliver_email("one", registration_email(&registration(1)), &address())
            .unwrap_err()
            .downcast_ref::<Fault>(),
        Some(Fault::Invalid)
    ));
    let admission = authority
        .deliver_email("one", registration_email(&registration(2)), &address())
        .unwrap();
    assert_eq!(admission.1.len(), 6);
    assert!(admission.1.bytes().all(|byte| byte.is_ascii_digit()));
    assert!(
        authority
            .confirm_email(&secret, address(), &admission.0.id, &admission.1)
            .is_err()
    );
    let expired = authority
        .deliver_email("one", profile_email(&secret), &address())
        .unwrap();
    authority.environment.now = expired.0.expires_at;
    assert!(
        authority
            .confirm_email(&secret, address(), &expired.0.id, &expired.1)
            .is_err()
    );
    let code = authority
        .deliver_email("one", profile_email(&secret), &address())
        .unwrap();
    for _ in 0..5 {
        assert!(
            authority
                .confirm_email(&secret, address(), &code.0.id, "wrong")
                .is_err()
        );
    }
    assert!(
        authority
            .confirm_email(&secret, address(), &code.0.id, &code.1)
            .is_err()
    );
    let id = authority.refresh(&secret).unwrap().credential;
    authority.revoke_credential(&secret, id).unwrap();
    assert!(
        authority
            .deliver_email("one", profile_email(&secret), &address())
            .is_err()
    );
    assert!(authority.set_nickname(&secret, "wind").is_err());
}

#[test]
fn email_delivery_throttles_atomically_by_recipient_account_and_global_buckets() {
    let mut authority = fixture();
    let mut settings = authority.settings().unwrap();
    settings.registration = RegistrationPolicy::Email;
    save(&mut authority.store, "settings", &settings).unwrap();

    // Rotating source IPs across canonical aliases hits the per-recipient bucket.
    for (idx, alias) in [
        "victim@gmail.com",
        "V.i.c.t.i.m+1@googlemail.com",
        "victim+2@gmail.com",
    ]
    .into_iter()
    .enumerate()
    {
        let attempt = registration(10 + u8::try_from(idx).unwrap());
        let address = Mailbox::parse(alias).unwrap();
        let (_, code) = authority
            .deliver_email(&format!("ip-{idx}"), registration_email(&attempt), &address)
            .unwrap();
        assert_eq!(code.len(), 6);
        assert!(code.bytes().all(|byte| byte.is_ascii_digit()));
    }
    let blocked = authority
        .deliver_email(
            "innocent-ip",
            registration_email(&registration(14)),
            &Mailbox::parse("v.i.c.t.i.m+3@gmail.com").unwrap(),
        )
        .unwrap_err();
    assert!(matches!(
        blocked.downcast_ref::<Fault>(),
        Some(Fault::RateLimited { retry_after: 600 })
    ));
    // Rejection on email-recipient must not drain the caller's source bucket.
    for idx in 0..3u8 {
        let address = Mailbox::parse(&format!("other{idx}@qq.com")).unwrap();
        authority
            .deliver_email(
                "innocent-ip",
                registration_email(&registration(20 + idx)),
                &address,
            )
            .unwrap();
    }

    // Account profile delivery throttles one account rotating across IPs and addresses.
    settings.registration = RegistrationPolicy::Automatic;
    save(&mut authority.store, "settings", &settings).unwrap();
    let secret = registration(30).client_secret;
    authority.register("acct-seed", registration(30)).unwrap();
    for idx in 0..3u8 {
        let address = Mailbox::parse(&format!("acct{idx}@163.com")).unwrap();
        authority
            .deliver_email(&format!("acct-ip-{idx}"), profile_email(&secret), &address)
            .unwrap();
    }
    let acct_limited = authority
        .deliver_email(
            "acct-ip-extra",
            profile_email(&secret),
            &Mailbox::parse("acct9@163.com").unwrap(),
        )
        .unwrap_err();
    assert!(matches!(
        acct_limited.downcast_ref::<Fault>(),
        Some(Fault::RateLimited { retry_after: 300 })
    ));

    // Exhaust the remaining global delivery burst (60 total; 9 spent above).
    settings.registration = RegistrationPolicy::Email;
    save(&mut authority.store, "settings", &settings).unwrap();
    for idx in 0..51u8 {
        let address = Mailbox::parse(&format!("global{idx}@qq.com")).unwrap();
        authority
            .deliver_email(
                &format!("global-ip-{idx}"),
                registration_email(&registration(40 + idx)),
                &address,
            )
            .unwrap();
    }
    let global_limited = authority
        .deliver_email(
            "fresh-ip",
            registration_email(&registration(95)),
            &Mailbox::parse("fresh@qq.com").unwrap(),
        )
        .unwrap_err();
    assert!(matches!(
        global_limited.downcast_ref::<Fault>(),
        Some(Fault::RateLimited { retry_after: 10 })
    ));
    authority.environment.now += 10;
    authority
        .deliver_email(
            "fresh-ip",
            registration_email(&registration(95)),
            &Mailbox::parse("fresh@qq.com").unwrap(),
        )
        .unwrap();
}

#[test]
fn canonical_email_uniqueness_protects_profiles_and_releases_moved_addresses() {
    let mut authority = fixture();
    let first = registration(1).client_secret;
    let second = registration(2).client_secret;
    authority.register("one", registration(1)).unwrap();
    authority.register("two", registration(2)).unwrap();

    // Account 1 binds alice@gmail.com (confirm accepts trimmed whitespace around the 6-digit OTP).
    let gmail = Mailbox::parse("alice@gmail.com").unwrap();
    let (challenge, code) = authority
        .deliver_email("one", profile_email(&first), &gmail)
        .unwrap();
    authority
        .confirm_email(&first, gmail, &challenge.id, &format!("  {code}\n"))
        .unwrap();

    // Account 2 cannot enumerate or claim an alias of Account 1's canonical mailbox.
    let alias = Mailbox::parse("A.l.i.c.e+dup@googlemail.com").unwrap();
    let (dup_challenge, dup_code) = authority
        .deliver_email("two", profile_email(&second), &alias)
        .unwrap();
    assert!(matches!(
        authority
            .confirm_email(&second, alias.clone(), &dup_challenge.id, "000000")
            .unwrap_err()
            .downcast_ref::<Fault>(),
        Some(Fault::EmailCodeRejected)
    ));
    assert!(matches!(
        authority
            .confirm_email(&second, alias, &dup_challenge.id, &dup_code)
            .unwrap_err()
            .downcast_ref::<Fault>(),
        Some(Fault::Denied { reason }) if reason == "email_taken"
    ));
    assert!(authority.refresh(&second).unwrap().profile.email.is_none());

    // Account 1 may update its own display variant and release the old canonical mailbox on change.
    let self_variant = Mailbox::parse("Alice+desk@gmail.com").unwrap();
    let (var_challenge, var_code) = authority
        .deliver_email("one", profile_email(&first), &self_variant)
        .unwrap();
    let updated = authority
        .confirm_email(&first, self_variant, &var_challenge.id, &var_code)
        .unwrap();
    assert_eq!(updated.email.as_deref(), Some("Alice+desk@gmail.com"));

    let moved = Mailbox::parse("alice@163.com").unwrap();
    let (move_challenge, move_code) = authority
        .deliver_email("one", profile_email(&first), &moved)
        .unwrap();
    authority
        .confirm_email(&first, moved, &move_challenge.id, &move_code)
        .unwrap();
    authority.environment.now += 600;
    let reclaim = Mailbox::parse("alice@gmail.com").unwrap();
    let (reclaim_challenge, reclaim_code) = authority
        .deliver_email("two", profile_email(&second), &reclaim)
        .unwrap();
    let claimed = authority
        .confirm_email(&second, reclaim, &reclaim_challenge.id, &reclaim_code)
        .unwrap();
    assert_eq!(claimed.email.as_deref(), Some("alice@gmail.com"));
}

fn verified_email_registration(
    authority: &mut Authority<Memory, Clock>,
    source: &str,
    secret_byte: u8,
    address: &str,
) -> Registration {
    let request = registration(secret_byte);
    let mailbox = Mailbox::parse(address).unwrap();
    let (challenge, code) = authority
        .deliver_email(source, registration_email(&request), &mailbox)
        .unwrap();
    Registration {
        challenge: Some(challenge.id),
        solution: Some(format!(" {code} ")),
        ..request
    }
}

#[test]
fn email_registration_recovers_existing_accounts_and_blocks_evasion() {
    let mut authority = fixture();
    let admin = authority
        .initialize("init", "init", "admin", "correct horse battery")
        .unwrap();
    let mut settings = authority.settings().unwrap();
    settings.registration = RegistrationPolicy::Email;
    save(&mut authority.store, "settings", &settings).unwrap();

    let first_request = verified_email_registration(&mut authority, "home", 1, "alice@gmail.com");
    let first_session = authority.register("home", first_request.clone()).unwrap();
    authority
        .set_nickname(&first_request.client_secret, "Alice")
        .unwrap();

    // Passwordless registration with a canonical alias recovers Account 1 with a sibling credential.
    let rec_request = verified_email_registration(
        &mut authority,
        "laptop",
        3,
        "a.l.i.c.e+laptop@googlemail.com",
    );
    let recovered = authority.register("laptop", rec_request).unwrap();
    assert_eq!(recovered.account, first_session.account);
    assert_ne!(recovered.credential, first_session.credential);
    assert_eq!(authority.account_count().unwrap(), 1);
    assert_eq!(
        recovered
            .profile
            .nickname
            .as_ref()
            .map(|n| n.value.as_str()),
        Some("Alice")
    );
    let retry = authority.register("laptop", registration(3)).unwrap();
    assert_eq!(retry.account, recovered.account);
    assert_eq!(retry.credential, recovered.credential);

    // Password registration refuses an already-bound canonical email atomically.
    let pw_request =
        verified_email_registration(&mut authority, "pw-ip", 4, "alice+pw@googlemail.com");
    let pw_commit = Registration {
        username: Some("newuser".into()),
        password_verifier: Some(test_verifier("correct horse battery")),
        ..pw_request
    };
    assert!(matches!(
        authority
            .register("pw-ip", pw_commit)
            .unwrap_err()
            .downcast_ref::<Fault>(),
        Some(Fault::Denied { reason }) if reason == "email_taken"
    ));

    // Blocking Account 1 stops email recovery and burns the OTP so it cannot replay after unblock.
    authority
        .block(
            &admin,
            Block::Account {
                id: first_session.account,
                blocked: true,
            },
        )
        .unwrap();
    authority.environment.now += 600;
    let evade =
        verified_email_registration(&mut authority, "evade-ip", 5, "a.l.i.c.e+evade@gmail.com");
    assert!(matches!(
        authority
            .register("evade-ip", evade.clone())
            .unwrap_err()
            .downcast_ref::<Fault>(),
        Some(Fault::Denied { reason }) if reason == "account_blocked"
    ));
    authority
        .block(
            &admin,
            Block::Account {
                id: first_session.account,
                blocked: false,
            },
        )
        .unwrap();
    assert!(matches!(
        authority
            .register("evade-ip", evade)
            .unwrap_err()
            .downcast_ref::<Fault>(),
        Some(Fault::Verification { challenge }) if challenge.id.is_empty()
    ));
}
