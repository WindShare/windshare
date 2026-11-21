use super::*;
use crate::public_blocks::{PublicBlockFeed, RENEW_AFTER_SECONDS};
use serde_json::json;
use windshare_relay_access::usage::ShareAttribution;

fn public(authority: &Authority<Memory, Clock>) -> serde_json::Value {
    let mut feed = PublicBlockFeed::default();
    let envelope: serde_json::Value =
        serde_json::from_str(&feed.snapshot(authority).unwrap().body).unwrap();
    let signed = URL_SAFE_NO_PAD
        .decode(envelope["body"].as_str().unwrap())
        .unwrap();
    serde_json::from_slice(
        trust::verify(
            b"windshare/public-blocks/v1\0",
            &signed,
            &authority.signing.generation.key,
        )
        .unwrap(),
    )
    .unwrap()
}

#[test]
fn policy_updates_merge_only_specified_fields() {
    let mut authority = fixture();
    let admin = authority
        .initialize("once", "once", "admin", "correct horse battery")
        .unwrap();
    let initial = authority.settings().unwrap();
    let parse = |body| serde_json::from_value::<SettingsChanges>(body).unwrap();

    let refill_seconds = initial.source_rate.refill_seconds + 1;
    let admission = parse(json!({
        "registration": "proof_of_work",
        "source_rate": { "refill_seconds": refill_seconds },
    }));
    authority.update_settings(&admin, &admission).unwrap();

    let daily_bytes = initial.daily_bytes + 1;
    let quota = parse(json!({ "daily_bytes": daily_bytes }));
    let (after_quota, mode) = authority.update_settings(&admin, &quota).unwrap();
    assert_eq!(mode, RegistrationMode::ProofOfWork);
    assert_eq!(after_quota.registration, RegistrationPolicy::ProofOfWork);
    assert_eq!(after_quota.daily_bytes, daily_bytes);
    assert_eq!(after_quota.pow_bits, initial.pow_bits);

    let burst = initial.source_rate.burst + 1;
    let rate = parse(json!({ "source_rate": { "burst": burst } }));
    let (saved, _) = authority.update_settings(&admin, &rate).unwrap();
    assert_eq!(
        saved,
        Settings {
            source_rate: Rate {
                burst,
                refill_seconds
            },
            ..after_quota
        }
    );

    let invalid = parse(json!({ "daily_bytes": daily_bytes + 1, "global_rate": { "burst": 0 } }));
    let error = authority.update_settings(&admin, &invalid).unwrap_err();
    assert!(matches!(
        error.downcast_ref::<Fault>(),
        Some(Fault::Invalid)
    ));
    assert_eq!(authority.settings().unwrap(), saved);
    assert!(serde_json::from_value::<SettingsChanges>(json!({ "daily_bytes": null })).is_err());
}

#[test]
fn node_policy_versions_survive_same_second_changes_and_disabled_reports() {
    let mut authority = fixture();
    let admin = authority
        .initialize("once", "once", "admin", "correct horse battery")
        .unwrap();
    let node = SigningKey::from_bytes(&[43; 32]);
    let key = node.verifying_key().to_bytes();
    let enrollment = |zone| NodeEnrollment {
        key,
        zone,
        region: "cn-test".into(),
    };
    let enrolled = authority
        .enroll_node(&admin, &enrollment(Zone::Cn))
        .unwrap();
    assert!(
        enrolled.admission == windshare_relay_access::usage::Admission::Accepting
            && !enrolled.anonymous
    );
    let account = authority
        .register("source", registration(1))
        .unwrap()
        .account;
    let bytes = batch(&authority, &node, account, 1, 2, 0);
    assert_eq!(status(&mut authority, &bytes).1.version, 1);
    authority
        .set_node_policy(
            &admin,
            &key,
            NodePolicyChange::Admission(windshare_relay_access::usage::Admission::Disabled),
        )
        .unwrap();
    let disabled = status(&mut authority, &bytes).1;
    assert_eq!(disabled.version, 2);
    assert_eq!(
        disabled.admission,
        windshare_relay_access::usage::Admission::Disabled
    );
    let existing = authority
        .enroll_node(&admin, &enrollment(Zone::Global))
        .unwrap();
    assert_eq!(existing.zone, Zone::Cn);
    assert!(
        existing.admission == windshare_relay_access::usage::Admission::Disabled
            && !existing.anonymous
    );
    let changed = authority
        .set_node_policy(&admin, &key, NodePolicyChange::AnonymousSharing(true))
        .unwrap();
    assert!(
        changed.admission == windshare_relay_access::usage::Admission::Disabled
            && changed.anonymous
    );
    assert_eq!(changed.version, 3);
    let repeated = authority
        .set_node_policy(&admin, &key, NodePolicyChange::AnonymousSharing(true))
        .unwrap();
    assert_eq!(repeated.version, 3);
    authority
        .set_node_policy(
            &admin,
            &key,
            NodePolicyChange::Admission(windshare_relay_access::usage::Admission::Accepting),
        )
        .unwrap();
    let restored = status(&mut authority, &bytes).1;
    assert_eq!(restored.version, 4);
    assert!(
        restored.admission == windshare_relay_access::usage::Admission::Accepting
            && restored.anonymous
    );
}

#[test]
fn account_blocks_cover_observed_shares_without_polluting_explicit_share_blocks() {
    let mut authority = fixture();
    let admin = authority
        .initialize("once", "once", "admin", "correct horse battery")
        .unwrap();
    let account = authority
        .register("source", registration(1))
        .unwrap()
        .account;
    let first = ShareAttribution {
        share: [1; 12],
        account,
    };
    authority.remember_shares(&[first]).unwrap();
    assert_eq!(public(&authority)["version"], 0);
    authority
        .block(
            &admin,
            Block::Account {
                id: account,
                blocked: true,
            },
        )
        .unwrap();
    let blocked = public(&authority);
    assert_eq!(
        blocked["shares"],
        serde_json::to_value(vec![[1; 12]]).unwrap()
    );
    let second = ShareAttribution {
        share: [2; 12],
        account,
    };
    authority.remember_shares(&[second]).unwrap();
    let expanded = public(&authority);
    assert!(expanded["version"].as_u64().unwrap() > blocked["version"].as_u64().unwrap());
    authority.remember_shares(&[first, second]).unwrap();
    assert_eq!(public(&authority), expanded);
    authority
        .block(
            &admin,
            Block::Share {
                id: first.share,
                blocked: true,
            },
        )
        .unwrap();
    authority
        .block(
            &admin,
            Block::Account {
                id: account,
                blocked: false,
            },
        )
        .unwrap();
    assert_eq!(
        public(&authority)["shares"],
        serde_json::to_value(vec![[1; 12]]).unwrap()
    );
    assert_eq!(authority.blocks().unwrap().shares.len(), 1);
}

#[test]
fn public_snapshot_reuses_bytes_until_renewal_or_policy_change() {
    let mut authority = fixture();
    let admin = authority
        .initialize("once", "once", "admin", "correct horse battery")
        .unwrap();
    let mut feed = PublicBlockFeed::default();
    let initial = feed.snapshot(&authority).unwrap();
    let initial_tag = initial.etag.clone();
    let initial_body = initial.body.clone();
    let writes = authority.store.put_count;

    authority.environment.now += RENEW_AFTER_SECONDS - 1;
    let reused = feed.snapshot(&authority).unwrap();
    assert!(reused.not_modified(&format!("W/{initial_tag}")));
    assert_eq!(reused.body, initial_body);

    authority.environment.now += 1;
    let renewed = feed.snapshot(&authority).unwrap();
    assert!(!renewed.not_modified(&initial_tag));
    let renewed_tag = renewed.etag.clone();
    assert_eq!(authority.store.put_count, writes);

    authority
        .block(
            &admin,
            Block::Share {
                id: [7; 12],
                blocked: true,
            },
        )
        .unwrap();
    let updated = feed.snapshot(&authority).unwrap();
    assert!(!updated.not_modified(&renewed_tag));
    let updated_tag = updated.etag.clone();

    authority.signing = signing_fixture(2);
    let rotated = feed.snapshot(&authority).unwrap();
    assert!(!rotated.not_modified(&updated_tag));
}

#[test]
fn reports_remain_reachable_beyond_the_old_console_limit() {
    let mut authority = fixture();
    let admin = authority
        .initialize("once", "once", "admin", "correct horse battery")
        .unwrap();
    let now = authority.environment.now;
    for i in 0..1000u128 {
        let id = i.to_be_bytes();
        let report = Report {
            id,
            share: [1; 12],
            reason: "older report".into(),
            created_at: now - 1 - u64::from(i == 0),
        };
        save(&mut authority.store, &key("report", &id), &report).unwrap();
    }
    let latest = authority
        .report("source", [2; 12], "new report".into())
        .unwrap();
    let oldest = [0; 16];
    let mut page = authority.reports(&admin, None).unwrap();
    assert_eq!(page.reports.len(), 50);
    assert_eq!(page.reports[0].id, latest);
    let mut seen = std::collections::BTreeSet::new();
    loop {
        for report in page.reports {
            assert!(seen.insert(report.id));
        }
        let Some(next) = page.next else { break };
        page = authority.reports(&admin, Some(next)).unwrap();
    }
    assert_eq!(seen.len(), 1001);
    assert!(seen.contains(&oldest));
    assert_eq!(authority.report_by_id(&admin, oldest).unwrap().id, oldest);
    assert!(matches!(
        authority
            .report_by_id(&admin, [255; 16])
            .err()
            .unwrap()
            .downcast_ref(),
        Some(Fault::ReportNotFound)
    ));
    assert!(authority.report_by_id("", oldest).is_err());
    assert!(authority.reports("", None).is_err());
    assert!(authority.console(&admin).unwrap().get("reports").is_none());
}

#[test]
fn node_details_and_load_remain_reachable_beyond_the_console_page() {
    let mut authority = fixture();
    let admin = authority
        .initialize("once", "once", "admin", "correct horse battery")
        .unwrap();
    for i in 1..=1001u16 {
        let mut id = [0; 32];
        id[..2].copy_from_slice(&i.to_be_bytes());
        authority
            .enroll_node(
                &admin,
                &NodeEnrollment {
                    key: id,
                    zone: Zone::Global,
                    region: "local".into(),
                },
            )
            .unwrap();
    }

    let snapshot = authority.console(&admin).unwrap();
    let first = &snapshot["node_page"];
    assert_eq!(first["entries"].as_array().unwrap().len(), 1000);
    let cursor: [u8; 32] = URL_SAFE_NO_PAD
        .decode(first["next_cursor"].as_str().unwrap())
        .unwrap()
        .try_into()
        .unwrap();
    let mut last = [0; 32];
    last[0] = 255;
    // Insert a key that sorts after every seeded node's encoded public key.
    authority
        .enroll_node(
            &admin,
            &NodeEnrollment {
                key: last,
                zone: Zone::Cn,
                region: "additional".into(),
            },
        )
        .unwrap();
    let now = authority.environment.now;
    let load = Load {
        relay_budget: None,
        applied_policy_version: 0,
        resident_shares: 0,
        active_sessions: 4,
        wire_bytes: 20,
        forwarded_bytes: 17,
        admission_refusals: 1,
    };
    save(
        &mut authority.store,
        &key("node-load", &last),
        &NodeLoadReport {
            created_at: now - 1,
            reported_at: now,
            load: load.clone(),
        },
    )
    .unwrap();

    let next = authority.nodes(&admin, Some(cursor)).unwrap();
    assert_eq!(next.entries.len(), 2);
    assert!(next.next_cursor.is_none());
    let row = next
        .entries
        .iter()
        .find(|entry| entry.node.key == last)
        .unwrap();
    let detail = authority.node_by_key(&admin, &last).unwrap();
    let report = detail.report.unwrap();
    assert_eq!(detail.node.zone, Zone::Cn);
    assert_eq!(report.created_at, now - 1);
    assert_eq!(report.reported_at, now);
    assert_eq!(report.load.forwarded_bytes, load.forwarded_bytes);
    assert_eq!(row.report.as_ref().unwrap().reported_at, report.reported_at);
    assert!(authority.nodes("", None).is_err());
    assert!(authority.node_by_key("", &last).is_err());
    assert!(matches!(
        authority
            .node_by_key(&admin, &[99; 32])
            .err()
            .unwrap()
            .downcast_ref(),
        Some(Fault::NodeNotFound)
    ));
}

#[test]
fn admin_password_change_invalidates_prior_sessions_and_rejects_stale_verifiers() {
    let mut authority = fixture();
    let first = authority
        .initialize("once", "once", "admin", "correct horse battery")
        .unwrap();
    let second = authority
        .admin_login("source", "admin", "correct horse battery")
        .unwrap();
    assert!(
        authority
            .prepare_admin_password_change("source", &first, 4)
            .unwrap_err()
            .downcast_ref::<Fault>()
            .is_some_and(|fault| matches!(fault, Fault::Invalid))
    );
    let verifier = authority
        .prepare_admin_password_change("source", &first, "updated-admin-password".len())
        .unwrap();
    assert!(verifier.verify("correct horse battery"));
    let updated = authority.hash_password("updated-admin-password").unwrap();
    assert!(
        authority
            .commit_admin_password_change(&first, updated.clone(), [0; 32])
            .unwrap_err()
            .downcast_ref::<Fault>()
            .is_some_and(
                |fault| matches!(fault, Fault::Denied { reason } if reason == "credentials_changed")
            )
    );
    let replacement = authority
        .commit_admin_password_change(&first, updated, verifier.fingerprint())
        .unwrap();
    assert!(authority.require_admin(&first).is_err());
    assert!(authority.require_admin(&second).is_err());
    authority.require_admin(&replacement).unwrap();
    assert!(
        authority
            .admin_login("source", "admin", "correct horse battery")
            .is_err()
    );
    let signed_in = authority
        .admin_login("source", "admin", "updated-admin-password")
        .unwrap();
    authority.require_admin(&signed_in).unwrap();
}

#[test]
fn milestone_reviews_include_thresholds_and_can_be_dismissed() {
    let mut authority = fixture();
    let admin = authority
        .initialize("once", "once", "admin", "correct horse battery")
        .unwrap();
    authority
        .update_settings(
            &admin,
            &SettingsChanges {
                registration: Some(RegistrationPolicy::Automatic),
                ..SettingsChanges::default()
            },
        )
        .unwrap();
    save(&mut authority.store, "account-count", &999u64).unwrap();
    authority.register("source-a", registration(1)).unwrap();
    authority.environment.now += 10;
    save(&mut authority.store, "account-count", &3_999u64).unwrap();
    authority.register("source-b", registration(2)).unwrap();

    let reviews: Vec<MilestoneReview> =
        serde_json::from_value(authority.console(&admin).unwrap()["reviews"].clone()).unwrap();
    assert_eq!(reviews.len(), 2);
    assert_eq!(reviews[0].count, 1_000);
    assert_eq!(reviews[1].count, 4_000);
    assert_eq!(reviews[1].reached_at, reviews[0].reached_at + 10);

    let remaining = authority.dismiss_review(&admin, 1_000).unwrap();
    assert_eq!(remaining, vec![reviews[1]]);
    assert_eq!(authority.dismiss_review(&admin, 4_000).unwrap(), Vec::new());
    assert!(authority.dismiss_review(&admin, 0).is_err());
    assert!(authority.dismiss_review("", 1_000).is_err());
}
