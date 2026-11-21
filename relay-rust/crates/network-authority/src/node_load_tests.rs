use super::*;
use crate::fleet::{
    NodeState,
    maintenance::Operation,
    topology::{Role, Spec, Status},
};
use windshare_relay_access::usage::REPORT_INTERVAL_SECONDS;

fn draining_node(key: [u8; 32], operation_id: &str) -> Node {
    let encoded_key = URL_SAFE_NO_PAD.encode(key);
    let mut node = Node::new(
        Spec {
            node_key: encoded_key,
            alias: "skew-relay".into(),
            relay: "https://skew-relay.example".into(),
            roles: vec![Role::Relay],
            stun: vec![],
            zone: Zone::Global,
            region: "skew-test".into(),
            provider: "test".into(),
            operator: "official".into(),
            capacity_mbps: 100,
        },
        operation_id.into(),
        NodeState::Listed {
            status: Status::Draining,
            withdrawn: std::collections::BTreeSet::new(),
        },
        2,
    )
    .unwrap();
    node.update_lifecycle(|n| {
        *n.maintenance = Some(operation_id.into());
        Ok(())
    })
    .unwrap();
    node
}

fn sign_report(
    auth: &Authority<Memory, Clock>,
    node: &SigningKey,
    id: u8,
    created_at: u64,
    applied_policy_version: u64,
    resident_shares: u64,
    records: Vec<UsageRecord>,
) -> Vec<u8> {
    let report = Batch {
        id: [id; 16],
        node: node.verifying_key().to_bytes(),
        issuer: auth.signing.generation.issuer,
        created_at,
        records,
        resident_accounts: vec![],
        shares: vec![],
        load: Load {
            relay_budget: None,
            applied_policy_version,
            resident_shares,
            active_sessions: 0,
            wire_bytes: 0,
            forwarded_bytes: 0,
            admission_refusals: 0,
        },
    };
    trust::sign(USAGE_DOMAIN, &serde_json::to_vec(&report).unwrap(), node)
}

#[test]
fn skewed_node_clocks_confirm_drain_on_receipt_time_and_survive_ntp_step_back() {
    let mut authority = fixture();
    let admin = authority
        .initialize("once", "once", "admin", "correct horse battery")
        .unwrap();
    let node = SigningKey::from_bytes(&[19; 32]);
    let node_key = node.verifying_key().to_bytes();
    let op = Operation {
        node_key: URL_SAFE_NO_PAD.encode(node_key),
        operation_id: "rolling-upgrade-01".into(),
    };
    save(
        &mut authority.store,
        &key("node", &node_key),
        &draining_node(node_key, &op.operation_id),
    )
    .unwrap();

    // Slow node clock (-240 s, inside +-300 s skew but beyond the 120 s freshness window).
    let slow_created = authority.environment.now - 240;
    let slow_receipt = authority.environment.now;
    let signed = sign_report(&authority, &node, 1, slow_created, 2, 0, vec![]);
    let _ = status(&mut authority, &signed);
    let detail = authority.node_by_key(&admin, &node_key).unwrap();
    assert!(detail.cutoff_ready);
    assert!(authority.maintenance_status(&op).unwrap().confirmed);
    assert_eq!(detail.report.as_ref().unwrap().created_at, slow_created);
    assert_eq!(detail.report.as_ref().unwrap().reported_at, slow_receipt);
    authority.environment.now += 2 * REPORT_INTERVAL_SECONDS;
    assert!(
        authority
            .node_by_key(&admin, &node_key)
            .unwrap()
            .cutoff_ready
    );
    assert!(authority.maintenance_status(&op).unwrap().confirmed);
    authority.environment.now += 1;
    assert!(
        !authority
            .node_by_key(&admin, &node_key)
            .unwrap()
            .cutoff_ready
    );
    assert!(!authority.maintenance_status(&op).unwrap().confirmed);

    // Fast node clock (+240 s) confirms immediately while alive, and expires on
    // authority receipt time instead of resurrecting after the node goes silent.
    let fast_created = authority.environment.now + 240;
    let signed = sign_report(&authority, &node, 2, fast_created, 2, 0, vec![]);
    let _ = status(&mut authority, &signed);
    assert!(
        authority
            .node_by_key(&admin, &node_key)
            .unwrap()
            .cutoff_ready
    );
    assert!(authority.maintenance_status(&op).unwrap().confirmed);
    authority.environment.now += 2 * REPORT_INTERVAL_SECONDS + 1;
    assert!(fast_created.abs_diff(authority.environment.now) <= 2 * REPORT_INTERVAL_SECONDS);
    assert!(
        !authority
            .node_by_key(&admin, &node_key)
            .unwrap()
            .cutoff_ready
    );
    assert!(!authority.maintenance_status(&op).unwrap().confirmed);

    // Backward NTP step: a newer heartbeat with a smaller node created_at still
    // updates applied_policy_version and resident_shares instead of sticking on old load.
    let pre_step_created = authority.environment.now + 200;
    let signed = sign_report(&authority, &node, 3, pre_step_created, 2, 3, vec![]);
    let _ = status(&mut authority, &signed);
    assert!(
        !authority
            .node_by_key(&admin, &node_key)
            .unwrap()
            .cutoff_ready
    );
    authority
        .set_node_policy(&admin, &node_key, NodePolicyChange::AnonymousSharing(false))
        .unwrap();
    authority.environment.now += REPORT_INTERVAL_SECONDS;
    let stepped_created = authority.environment.now;
    assert!(stepped_created < pre_step_created);
    let signed = sign_report(&authority, &node, 4, stepped_created, 3, 0, vec![]);
    let _ = status(&mut authority, &signed);
    let detail = authority.node_by_key(&admin, &node_key).unwrap();
    let report = detail.report.unwrap();
    assert_eq!(report.created_at, stepped_created);
    assert_eq!(report.reported_at, authority.environment.now);
    assert_eq!(report.load.applied_policy_version, 3);
    assert_eq!(report.load.resident_shares, 0);
    assert!(detail.cutoff_ready);
    assert!(authority.maintenance_status(&op).unwrap().confirmed);
}

#[test]
fn status_heartbeats_enforce_symmetric_clock_skew_while_queued_usage_retries_remain_accepted() {
    let mut authority = fixture();
    let admin = authority
        .initialize("once", "once", "admin", "correct horse battery")
        .unwrap();
    let session = authority.register("source", registration(1)).unwrap();
    let node = SigningKey::from_bytes(&[23; 32]);
    let node_key = node.verifying_key().to_bytes();
    save(
        &mut authority.store,
        &key("node", &node_key),
        &draining_node(node_key, "rolling-upgrade-02"),
    )
    .unwrap();

    let initial_receipt = authority.environment.now;
    let initial = sign_report(&authority, &node, 1, initial_receipt, 2, 0, vec![]);
    let _ = status(&mut authority, &initial);
    authority.environment.now += 2 * REPORT_INTERVAL_SECONDS + 1;
    assert!(
        !authority
            .node_by_key(&admin, &node_key)
            .unwrap()
            .cutoff_ready
    );

    let stale_status = sign_report(
        &authority,
        &node,
        2,
        authority.environment.now - trust::CLOCK_SKEW - 1,
        2,
        0,
        vec![],
    );
    let future_status = sign_report(
        &authority,
        &node,
        3,
        authority.environment.now + trust::CLOCK_SKEW + 1,
        2,
        0,
        vec![],
    );
    let events = observability::capture_events(|| {
        for rejected in [&stale_status, &future_status] {
            let fault = authority
                .usage(rejected)
                .unwrap_err()
                .downcast::<Fault>()
                .unwrap();
            assert!(matches!(fault, Fault::Invalid));
        }
    });
    let skew_events = observability::events_named(&events, "node_report_clock_skew");
    assert_eq!(skew_events.len(), 2);
    assert_eq!(skew_events[0]["fields"]["usage"], false);
    assert_eq!(skew_events[1]["fields"]["usage"], false);
    let detail = authority.node_by_key(&admin, &node_key).unwrap();
    assert_eq!(detail.report.unwrap().reported_at, initial_receipt);
    assert!(!detail.cutoff_ready);

    // Delayed accounting batches from an outage older than CLOCK_SKEW are still accepted.
    let outage_created = authority.environment.now - trust::CLOCK_SKEW - 3600;
    let queued_usage = sign_report(
        &authority,
        &node,
        4,
        outage_created,
        2,
        0,
        vec![UsageRecord {
            account: session.account,
            day: outage_created / DAY_SECONDS,
            bytes: 512,
        }],
    );
    assert!(matches!(
        reply(&mut authority, &queued_usage),
        ReportResult::UsageReceived
    ));
}
