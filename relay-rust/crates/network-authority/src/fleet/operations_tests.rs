use super::*;
use crate::fleet::{
    operations::{Action, Manage},
    publication,
    topology::{Role, Status},
};
use windshare_relay_access::usage::{Admission, NodePolicy, REPORT_INTERVAL_SECONDS};

fn command(node: &Node, action: Action) -> Manage {
    Manage {
        node_key: node.spec.node_key.clone(),
        expected_state: node.state(),
        expected_policy_version: node.version,
        action,
    }
}

fn assert_conflict<T: std::fmt::Debug>(result: Result<T>, expected: &str) {
    let err = result.unwrap_err();
    assert!(
        matches!(
            err.downcast_ref::<Fault>(),
            Some(Fault::Conflict { reason }) if reason == expected
        ),
        "expected Fault::Conflict({expected}), got {err:?}"
    );
}

fn assert_invalid<T: std::fmt::Debug>(result: Result<T>) {
    let err = result.unwrap_err();
    assert!(
        matches!(err.downcast_ref::<Fault>(), Some(Fault::Invalid)),
        "expected Fault::Invalid, got {err:?}"
    );
}

fn report_status(
    auth: &mut Authority<Memory, Clock>,
    node: &SigningKey,
    applied_policy_version: u64,
    resident_shares: u64,
) -> NodePolicy {
    auth.environment.now += 1;
    let id = auth.environment.random::<16>().unwrap();
    let batch = Batch {
        id,
        node: node.verifying_key().to_bytes(),
        issuer: auth.signing.generation.issuer,
        created_at: auth.environment.now,
        records: vec![],
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
    let signed = trust::sign(USAGE_DOMAIN, &serde_json::to_vec(&batch).unwrap(), node);
    let ReportResult::Status { policy, .. } = reply(auth, &signed) else {
        panic!("expected signed status policy");
    };
    policy
}

fn directory_status(store: &Memory, alias: &str) -> Option<Status> {
    publication::current(store)
        .unwrap()
        .payload()
        .unwrap()
        .nodes
        .into_iter()
        .find(|entry| entry.spec.alias == alias)
        .map(|entry| entry.status)
}

#[test]
fn enrollment_and_management_preserve_intent_aliases_and_policy_revisions() {
    let mut auth = fixture();
    let admin = auth
        .initialize("init", "init", "operator", "reasonable password")
        .unwrap();
    let (release, signer, history) = directory();
    Publication::bootstrap(&mut auth.store, &release, &[&history]).unwrap();
    let spec: Spec = serde_json::from_value(spec("new", 7)).unwrap();
    let node = auth.enroll(&admin, spec.clone(), &signer).unwrap();
    assert_eq!(node.state(), NodeState::Canary);
    assert_eq!(node.admission(), Admission::Accepting);
    assert!(!auth.node_by_key(&admin, &node.key).unwrap().report_stale);
    let activate = command(&node, Action::Activate);
    auth.manage(&admin, &activate, &signer).unwrap();
    let revision = publication::current(&auth.store)
        .unwrap()
        .revision()
        .unwrap()
        .serial;
    let active = auth.enroll(&admin, spec.clone(), &signer).unwrap();
    assert_eq!(active.state(), NodeState::Active);
    assert_eq!(
        publication::current(&auth.store)
            .unwrap()
            .revision()
            .unwrap()
            .serial,
        revision
    );
    assert_conflict(auth.manage(&admin, &activate, &signer), "listing_changed");
    let stale = command(&active, Action::Drain);
    let current = auth
        .set_node_policy(&admin, &node.key, NodePolicyChange::AnonymousSharing(true))
        .unwrap();
    assert_conflict(auth.manage(&admin, &stale, &signer), "listing_changed");
    auth.manage(&admin, &command(&current, Action::Drain), &signer)
        .unwrap();
    let draining = auth.node_by_key(&admin, &node.key).unwrap().node;
    assert_eq!(draining.admission(), Admission::Draining);
    assert_eq!(directory_status(&auth.store, "new"), Some(Status::Draining));

    // Reactivating a draining node resumes relay admission and republishes it as active.
    auth.manage(&admin, &command(&draining, Action::Activate), &signer)
        .unwrap();
    let resumed = auth.node_by_key(&admin, &node.key).unwrap().node;
    assert_eq!(resumed.state(), NodeState::Active);
    assert_eq!(resumed.admission(), Admission::Accepting);
    assert_eq!(resumed.version, draining.version + 1);
    assert_eq!(directory_status(&auth.store, "new"), Some(Status::Active));

    auth.manage(&admin, &command(&resumed, Action::Drain), &signer)
        .unwrap();
    let draining = auth.node_by_key(&admin, &node.key).unwrap().node;
    assert_conflict(
        auth.manage(
            &admin,
            &command(
                &draining,
                Action::Disable {
                    early_cutoff: false,
                },
            ),
            &signer,
        ),
        "drain_pending",
    );
    auth.manage(
        &admin,
        &command(&draining, Action::Disable { early_cutoff: true }),
        &signer,
    )
    .unwrap();
    let disabled = auth.node_by_key(&admin, &node.key).unwrap().node;
    assert_eq!(directory_status(&auth.store, "new"), None);

    // Reactivating a disabled node restores its directory entry and advances policy version.
    auth.manage(&admin, &command(&disabled, Action::Activate), &signer)
        .unwrap();
    let restored = auth.node_by_key(&admin, &node.key).unwrap().node;
    assert_eq!(restored.state(), NodeState::Active);
    assert_eq!(restored.admission(), Admission::Accepting);
    assert_eq!(restored.version, disabled.version + 1);
    assert_eq!(directory_status(&auth.store, "new"), Some(Status::Active));

    auth.manage(
        &admin,
        &command(&restored, Action::Disable { early_cutoff: true }),
        &signer,
    )
    .unwrap();
    let disabled = auth.node_by_key(&admin, &node.key).unwrap().node;
    assert_conflict(
        auth.manage(
            &admin,
            &command(
                &disabled,
                Action::Remove {
                    early_cutoff: false,
                },
            ),
            &signer,
        ),
        "drain_pending",
    );
    auth.manage(
        &admin,
        &command(&disabled, Action::Remove { early_cutoff: true }),
        &signer,
    )
    .unwrap();
    let reassigned = Spec {
        relay: "https://other.example".into(),
        ..spec.clone()
    };
    assert_conflict(auth.enroll(&admin, reassigned, &signer), "alias_bound");
    assert_eq!(
        auth.enroll(&admin, spec, &signer).unwrap().version,
        disabled.version + 1
    );
}

#[test]
fn graceful_disable_and_remove_succeed_after_fresh_zero_resident_heartbeat() {
    let mut auth = fixture();
    let admin = auth
        .initialize("init", "init", "operator", "reasonable password")
        .unwrap();
    let (release, signer, history) = directory();
    Publication::bootstrap(&mut auth.store, &release, &[&history]).unwrap();
    let signing_key = SigningKey::from_bytes(&[15; 32]);
    let mut node_spec: Spec = serde_json::from_value(spec("graceful", 15)).unwrap();
    node_spec.node_key = URL_SAFE_NO_PAD.encode(signing_key.verifying_key().to_bytes());
    let node = auth.enroll(&admin, node_spec, &signer).unwrap();
    auth.manage(&admin, &command(&node, Action::Drain), &signer)
        .unwrap();
    let draining = auth.node_by_key(&admin, &node.key).unwrap().node;

    let drain_policy = report_status(&mut auth, &signing_key, 0, 0);
    assert_eq!(drain_policy.version, draining.version);
    assert_eq!(drain_policy.admission, Admission::Draining);
    let disable = command(
        &draining,
        Action::Disable {
            early_cutoff: false,
        },
    );
    assert_conflict(auth.manage(&admin, &disable, &signer), "drain_pending");

    report_status(&mut auth, &signing_key, drain_policy.version, 1);
    assert_conflict(auth.manage(&admin, &disable, &signer), "drain_pending");

    report_status(&mut auth, &signing_key, drain_policy.version, 0);
    auth.environment.now += 2 * REPORT_INTERVAL_SECONDS + 1;
    assert_conflict(auth.manage(&admin, &disable, &signer), "drain_pending");

    report_status(&mut auth, &signing_key, drain_policy.version, 0);
    auth.manage(&admin, &disable, &signer).unwrap();
    let disabled = auth.node_by_key(&admin, &node.key).unwrap().node;
    assert_eq!(disabled.state(), NodeState::Disabled);
    assert_eq!(directory_status(&auth.store, "graceful"), None);

    let remove = command(
        &disabled,
        Action::Remove {
            early_cutoff: false,
        },
    );
    assert_conflict(auth.manage(&admin, &remove, &signer), "drain_pending");
    let disabled_policy = report_status(&mut auth, &signing_key, drain_policy.version, 0);
    assert_eq!(disabled_policy.admission, Admission::Disabled);
    assert_eq!(disabled_policy.version, disabled.version);
    report_status(&mut auth, &signing_key, disabled_policy.version, 0);
    auth.manage(&admin, &remove, &signer).unwrap();
    assert!(auth.store.get(&key("node", &node.key)).unwrap().is_none());
    assert!(
        auth.store
            .get(&key("node-load", &node.key))
            .unwrap()
            .is_none()
    );
    assert_eq!(
        report_status(&mut auth, &signing_key, 0, 0),
        disabled_policy
    );
}

#[test]
fn pure_stun_is_published_but_never_admits_relay_traffic() {
    let mut auth = fixture();
    let admin = auth
        .initialize("init", "init", "operator", "reasonable password")
        .unwrap();
    let (release, signer, _) = directory();
    Publication::bootstrap(&mut auth.store, &release, &[]).unwrap();
    let mut spec: Spec = serde_json::from_value(spec("stun", 42)).unwrap();
    spec.roles = vec![Role::Stun];
    spec.stun = vec!["stun:stun.example:3479".into()];
    let node = auth.enroll(&admin, spec, &signer).unwrap();
    assert_invalid(auth.manage(&admin, &command(&node, Action::Drain), &signer));
    auth.manage(&admin, &command(&node, Action::Activate), &signer)
        .unwrap();
    let active = auth.node_by_key(&admin, &node.key).unwrap().node;
    assert_eq!(active.admission(), Admission::Draining);
    assert_eq!(active.version, node.version);

    auth.manage(
        &admin,
        &command(&active, Action::Disable { early_cutoff: true }),
        &signer,
    )
    .unwrap();
    let disabled = auth.node_by_key(&admin, &node.key).unwrap().node;
    assert_eq!(directory_status(&auth.store, "stun"), None);
    auth.manage(&admin, &command(&disabled, Action::Activate), &signer)
        .unwrap();
    let restored = auth.node_by_key(&admin, &node.key).unwrap().node;
    assert_eq!(restored.admission(), Admission::Draining);
    assert_eq!(restored.version, disabled.version + 1);
    assert_eq!(directory_status(&auth.store, "stun"), Some(Status::Active));
}

#[test]
fn legacy_nodes_migrate_once_without_resigning_or_reusing_a_draft_version() {
    let mut store = Memory::default();
    let (release, _, history) = directory();
    let head = Publication::bootstrap(&mut store, &release, &[&history]).unwrap();
    let held_key = key("node", &[8; 32]);
    let mut upgrading = serde_json::to_value(listed_node([8; 32], Zone::Global, "held")).unwrap();
    upgrading["state"] = json!({"kind":"listed","status":"active","maintenance":"in-progress"});
    save(&mut store, &held_key, &upgrading).unwrap();
    assert!(crate::fleet::migration::migrate(&mut store).is_err());
    store.delete(&held_key).unwrap();
    for (id, state) in [
        (9, json!({"kind":"listed", "status":"active"})),
        (10, json!({"kind":"staging", "enrollment":"disabled"})),
        (11, json!({"kind":"staging", "enrollment":"provisional"})),
        (12, json!({"kind":"disabled"})),
    ] {
        let mut legacy = serde_json::to_value(listed_node([id; 32], Zone::Global, "test")).unwrap();
        legacy["state"] = state;
        legacy["operation_id"] = json!("legacy");
        legacy["version"] = json!(7);
        save(&mut store, &key("node", &[id; 32]), &legacy).unwrap();
    }
    let legacy_keys = [
        key("node-load", &[11; 32]),
        "vantage-lease/old".into(),
        "fleet-cycle/old".into(),
        "maintenance-complete/old".into(),
        "directory/mirrors".into(),
    ];
    for record in &legacy_keys {
        store.put(record, "{}").unwrap();
    }
    crate::fleet::migration::migrate(&mut store).unwrap();
    let active: Node = load(&store, &key("node", &[9; 32])).unwrap().unwrap();
    let staging_disabled: Node = load(&store, &key("node", &[10; 32])).unwrap().unwrap();
    let retired: NodePolicy = load(&store, &key("retired-node", &[11; 32]))
        .unwrap()
        .unwrap();
    let disabled: Node = load(&store, &key("node", &[12; 32])).unwrap().unwrap();
    assert_eq!((active.state(), active.version), (NodeState::Active, 7));
    assert_eq!(
        (staging_disabled.state(), staging_disabled.version),
        (NodeState::Disabled, 8)
    );
    assert_eq!(
        (retired.admission, retired.version),
        (Admission::Disabled, 8)
    );
    assert_eq!(
        (disabled.state(), disabled.version),
        (NodeState::Disabled, 7)
    );
    assert!(store.get(&key("node", &[11; 32])).unwrap().is_none());
    for record in &legacy_keys {
        assert!(
            store.get(record).unwrap().is_none(),
            "legacy key {record} was not cleaned up"
        );
    }
    assert_eq!(
        publication::current(&store).unwrap().envelope,
        head.envelope
    );
    let writes = store.put_count;
    crate::fleet::migration::migrate(&mut store).unwrap();
    assert_eq!(store.put_count, writes);
}

fn assert_correlated_operation_id(
    events: &[(tracing::Level, Value)],
    expected_events: &[&str],
) -> String {
    use super::super::observability::events_named;

    let mut shared: Option<&str> = None;
    for name in expected_events {
        let matched = events_named(events, name);
        assert_eq!(matched.len(), 1, "expected one {name} event");
        let fields = &matched[0]["fields"];
        assert!(
            fields.get("operation").is_none(),
            "{name} should not emit legacy operation field"
        );
        let operation_id = fields["operation_id"]
            .as_str()
            .expect("operation_id string");
        assert_ne!(operation_id, "");
        match shared {
            Some(previous) => assert_eq!(operation_id, previous),
            None => shared = Some(operation_id),
        }
    }
    shared.expect("at least one event").to_owned()
}

#[test]
fn fleet_mutation_traces_correlate_with_operation_id() {
    use super::super::observability::capture_events;

    let mut auth = fixture();
    let admin = auth
        .initialize("init", "init", "operator", "reasonable password")
        .unwrap();
    let (release, signer, history) = directory();
    Publication::bootstrap(&mut auth.store, &release, &[&history]).unwrap();
    let spec: Spec = serde_json::from_value(spec("traced", 17)).unwrap();

    let mut enrolled = None;
    let enroll_events = capture_events(|| {
        enrolled = Some(auth.enroll(&admin, spec, &signer).unwrap());
    });
    let node = enrolled.unwrap();
    let enroll_op = assert_correlated_operation_id(
        &enroll_events,
        &[
            "node_enrolled",
            "directory_entry_changed",
            "directory_signed",
        ],
    );

    let activate_events = capture_events(|| {
        auth.manage(&admin, &command(&node, Action::Activate), &signer)
            .unwrap();
    });
    let activate_op = assert_correlated_operation_id(
        &activate_events,
        &[
            "node_intent_changed",
            "directory_entry_changed",
            "directory_signed",
        ],
    );
    assert_ne!(activate_op, enroll_op);

    let active = auth.node_by_key(&admin, &node.key).unwrap().node;
    let disable_events = capture_events(|| {
        auth.manage(
            &admin,
            &command(&active, Action::Disable { early_cutoff: true }),
            &signer,
        )
        .unwrap();
    });
    let disable_op = assert_correlated_operation_id(
        &disable_events,
        &[
            "node_intent_changed",
            "directory_entry_removed",
            "directory_signed",
        ],
    );
    assert_ne!(disable_op, activate_op);

    let disabled = auth.node_by_key(&admin, &node.key).unwrap().node;
    let remove_events = capture_events(|| {
        auth.manage(
            &admin,
            &command(&disabled, Action::Remove { early_cutoff: true }),
            &signer,
        )
        .unwrap();
    });
    let remove_op = assert_correlated_operation_id(&remove_events, &["node_removed"]);
    assert_ne!(remove_op, disable_op);
}
