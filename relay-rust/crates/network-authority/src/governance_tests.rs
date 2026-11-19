use super::*;
use serde_json::json;
use windshare_relay_access::usage::ShareAttribution;

fn public(authority: &Authority<Memory, Clock>) -> serde_json::Value {
    let envelope = authority.public_blocks().unwrap();
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
    assert!(enrolled.enabled && !enrolled.anonymous);
    let account = authority
        .register("source", registration(1))
        .unwrap()
        .account;
    let bytes = batch(&authority, &node, account, 1, 2, 0);
    assert_eq!(status(&mut authority, &bytes).1.version, 1);
    authority
        .set_node_policy(&admin, &key, NodePolicyChange::PublicRelay(false))
        .unwrap();
    let disabled = status(&mut authority, &bytes).1;
    assert_eq!(disabled.version, 2);
    assert!(!disabled.enabled);
    let existing = authority
        .enroll_node(&admin, &enrollment(Zone::Global))
        .unwrap();
    assert_eq!(existing.zone, Zone::Cn);
    assert!(!existing.enabled && !existing.anonymous);
    let changed = authority
        .set_node_policy(&admin, &key, NodePolicyChange::AnonymousSharing(true))
        .unwrap();
    assert!(!changed.enabled && changed.anonymous);
    assert_eq!(changed.version, 3);
    let repeated = authority
        .set_node_policy(&admin, &key, NodePolicyChange::AnonymousSharing(true))
        .unwrap();
    assert_eq!(repeated.version, 3);
    authority
        .set_node_policy(&admin, &key, NodePolicyChange::PublicRelay(true))
        .unwrap();
    let restored = status(&mut authority, &bytes).1;
    assert_eq!(restored.version, 4);
    assert!(restored.enabled && restored.anonymous);
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
