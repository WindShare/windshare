use super::*;
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
fn latest_report_is_visible_after_the_console_limit() {
    let mut authority = fixture();
    let admin = authority
        .initialize("once", "once", "admin", "correct horse battery")
        .unwrap();
    for i in 0..1000u16 {
        let report = Report {
            id: [12; 16],
            share: [1; 12],
            reason: "older report".into(),
            created_at: authority.environment.now - 1,
        };
        save(&mut authority.store, &format!("report/{i:04}"), &report).unwrap();
    }
    let id = authority
        .report("source", [2; 12], "new report".into())
        .unwrap();
    let console = authority.console(&admin).unwrap();
    let reports = console["reports"].as_array().unwrap();
    assert_eq!(reports.len(), 1000);
    assert_eq!(reports[0]["id"], serde_json::to_value(id).unwrap());
}
