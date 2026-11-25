use super::*;
use windshare_relay_access::restrictions::Restriction;

const ADMIN: &str = "active-admin";

fn admin_fixture() -> Authority<Memory, Clock> {
    let mut authority = fixture();
    save(
        &mut authority.store,
        &key("admin-session", &crate::registration::digest(ADMIN)),
        &u64::MAX,
    )
    .unwrap();
    authority
}

#[test]
fn reads_require_a_session_before_validating_any_admin_input() {
    let authority = admin_fixture();
    for result in [
        authority.accounts("", Some("bad")).unwrap_err(),
        authority.lookup_account("", " ").unwrap_err(),
        authority.account_by_id("", "!").unwrap_err(),
    ] {
        assert!(
            matches!(result.downcast_ref::<Fault>(), Some(Fault::Denied { reason }) if reason == "administrator_session")
        );
    }
    for malformed in [
        "",
        "broken",
        "1.bad",
        "1.AAAAAAAAAAAAAAAAAAAAAA",
        "-1.AAAAAAAAAAAAAAAAAAAAAA",
        "1.AAAAAAAAAAAAAAAAAAAAAB",
    ] {
        assert!(matches!(
            authority
                .accounts(ADMIN, Some(malformed))
                .unwrap_err()
                .downcast_ref::<Fault>(),
            Some(Fault::Invalid)
        ));
    }
}

#[test]
fn immutable_registration_index_pages_stably_across_timestamp_ties_and_retries() {
    let mut authority = admin_fixture();
    let mut expected = Vec::new();
    for id in 1..=53 {
        if id == 52 {
            authority.environment.now += 1;
        }
        let created = authority
            .register(&id.to_string(), registration(id))
            .unwrap();
        expected.push((
            std::cmp::Reverse(authority.environment.now),
            URL_SAFE_NO_PAD.encode(created.account),
        ));
    }
    let retry = authority.register("retry", registration(1)).unwrap();
    save(
        &mut authority.store,
        &format!("{}/2", key("usage", &retry.account)),
        &DailyUsageRecord {
            bytes: 42,
            version: 1,
        },
    )
    .unwrap();
    let index = authority.store.scan("account-index/", None).unwrap();
    assert_eq!(index.len(), expected.len());
    assert!(index.iter().all(|(_, value)| value.is_empty()));
    let first = authority.accounts(ADMIN, None).unwrap();
    assert_eq!(first.today_day, 2);
    assert_eq!(first.entries.len(), 50);
    let second = authority
        .accounts(ADMIN, first.next_cursor.as_deref())
        .unwrap();
    assert!(second.next_cursor.is_none());
    let all: Vec<_> = first.entries.into_iter().chain(second.entries).collect();
    assert_eq!(
        all.iter()
            .find(|row| row.id == retry.account)
            .unwrap()
            .today_bytes,
        42
    );
    expected.sort();
    assert_eq!(
        all.iter()
            .map(|entry| URL_SAFE_NO_PAD.encode(entry.id))
            .collect::<Vec<_>>(),
        expected.into_iter().map(|(_, id)| id).collect::<Vec<_>>()
    );
    authority.environment.now += DAY_SECONDS;
    let tomorrow = authority.accounts(ADMIN, None).unwrap();
    assert_eq!(tomorrow.today_day, 3);
    assert!(tomorrow.entries.iter().all(|row| row.today_bytes == 0));
}

#[test]
fn lookup_prefers_exact_ids_and_falls_back_to_normalized_usernames_without_loading_history() {
    let mut authority = admin_fixture();
    let id_like_name = URL_SAFE_NO_PAD.encode([87; 16]);
    let account = authority
        .register_with_password(
            "ip",
            registration(1),
            &id_like_name,
            "sample-login-password",
        )
        .unwrap()
        .account;
    authority
        .store
        .put(
            &format!("{}/1", key("usage", &account)),
            "unreadable history",
        )
        .unwrap();
    let result = authority
        .lookup_account(ADMIN, &format!(" {id_like_name} "))
        .unwrap();
    assert!(result.has_password);
    assert_eq!(result.id, account);
    let json = serde_json::to_value(result).unwrap();
    assert_eq!(json.as_object().unwrap().len(), 7);
    assert!(json.get("password").is_none());
    let id_text = URL_SAFE_NO_PAD.encode(account);
    save(
        &mut authority.store,
        &format!("username/{}", id_text.to_ascii_lowercase()),
        &[5_u8; 16],
    )
    .unwrap();
    assert_eq!(
        authority.lookup_account(ADMIN, &id_text).unwrap().id,
        account
    );
    for query in ["a@example.net", "nonexistent", "名字"] {
        assert!(matches!(
            authority
                .lookup_account(ADMIN, query)
                .unwrap_err()
                .downcast_ref::<Fault>(),
            Some(Fault::AccountNotFound)
        ));
    }
    assert!(matches!(
        authority
            .lookup_account(ADMIN, " ")
            .unwrap_err()
            .downcast_ref::<Fault>(),
        Some(Fault::Invalid)
    ));
}

#[test]
fn blocked_account_diagnostics_are_read_only_numeric_and_bounded() {
    let mut authority = admin_fixture();
    let session = authority.register("ip", registration(1)).unwrap();
    let id = session.account;
    let text = URL_SAFE_NO_PAD.encode(id);
    assert!(
        authority
            .account_by_id(ADMIN, &text)
            .unwrap()
            .recent_usage
            .is_empty()
    );
    authority.environment.now = 110 * DAY_SECONDS;
    for day in 78..=110 {
        save(
            &mut authority.store,
            &format!("{}/{day}", key("usage", &id)),
            &DailyUsageRecord {
                bytes: day * 2,
                version: 1,
            },
        )
        .unwrap();
    }
    save(
        &mut authority.store,
        "settings",
        &Settings {
            daily_bytes: 100,
            ..Settings::default()
        },
    )
    .unwrap();
    authority
        .set_restriction(Restriction::Account(id), true)
        .unwrap();
    let writes = authority.store.put_count;
    let details = authority.account_by_id(ADMIN, &text).unwrap();
    assert!(details.restricted && details.has_older_usage);
    assert_eq!(
        (
            details.today_day,
            details.summary.today_bytes,
            details.daily_limit_bytes,
            details.remaining_bytes
        ),
        (110, 220, 100, 0)
    );
    assert_eq!(
        details.entitlement_version,
        authority.entitlement(id).unwrap().version
    );
    assert_eq!(details.entitlement_version, session.entitlement.version + 1);
    assert_eq!(
        details.restriction_version,
        authority.restriction_version().unwrap()
    );
    assert_eq!(
        details
            .recent_usage
            .iter()
            .map(|entry| entry.day)
            .collect::<Vec<_>>(),
        (81..=110).rev().collect::<Vec<_>>()
    );
    assert_eq!(authority.store.put_count, writes);
    assert!(matches!(
        authority
            .account_by_id(ADMIN, &URL_SAFE_NO_PAD.encode([99; 16]))
            .unwrap_err()
            .downcast_ref::<Fault>(),
        Some(Fault::AccountNotFound)
    ));
}
