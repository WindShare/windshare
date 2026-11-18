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
    let login = authority
        .set_password(&secret, "alice", "correct horse battery")
        .unwrap();
    assert_eq!(login.profile, profile);
    let sibling = authority.login("alice", "correct horse battery").unwrap();
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
fn provider_policy_matches_the_whole_domain_and_preserves_the_local_part() {
    for address in [
        "one@qq.com",
        "first.last+tag@163.com",
        "Test@GMAIL.COM",
        "me@outlook.com",
        "me@icloud.com",
    ] {
        assert!(Mailbox::parse(address).is_ok(), "{address}");
    }
    assert_eq!(
        Mailbox::parse("  Alice+tag@QQ.COM ").unwrap().as_str(),
        "Alice+tag@qq.com"
    );
    for address in [
        "a@example.com",
        "a@sub.qq.com",
        "a@qq.com.example",
        "a@qԛ.com",
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
            .deliver_email(source, Some(&first), &email)
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
    let admission = authority.deliver_email("one", None, &address()).unwrap();
    assert!(
        authority
            .confirm_email(&secret, address(), &admission.0.id, &admission.1)
            .is_err()
    );
    let expired = authority
        .deliver_email("one", Some(&secret), &address())
        .unwrap();
    authority.environment.now = expired.0.expires_at;
    assert!(
        authority
            .confirm_email(&secret, address(), &expired.0.id, &expired.1)
            .is_err()
    );
    let code = authority
        .deliver_email("one", Some(&secret), &address())
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
            .deliver_email("one", Some(&secret), &address())
            .is_err()
    );
    assert!(authority.set_nickname(&secret, "wind").is_err());
}
