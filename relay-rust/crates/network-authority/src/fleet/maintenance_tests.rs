use super::*;
use crate::fleet::{
    attach,
    maintenance::Operation,
    operations::{Action, Admission, Manage},
    publication,
    topology::{Entry, Role},
};
use tracing::Level;

struct MaintenanceFleet {
    auth: Authority<Memory, Clock>,
    admin: String,
    signer: Publisher,
    target: Spec,
    spare: Spec,
}

impl MaintenanceFleet {
    fn new() -> Self {
        let mut auth = fixture();
        let admin = auth
            .initialize("init", "init", "operator", "reasonable password")
            .unwrap();
        let (release, signer, history) = directory();
        Publication::bootstrap(&mut auth.store, &release, &[&history]).unwrap();
        let mut add = |alias, byte| {
            let mut value = spec(alias, byte);
            value["node_key"] = json!(
                URL_SAFE_NO_PAD.encode(
                    SigningKey::from_bytes(&[byte; 32])
                        .verifying_key()
                        .to_bytes()
                )
            );
            value["roles"] = json!(["relay", "stun"]);
            value["stun"] = json!([format!("stun:{alias}.example:3478")]);
            let node: Spec = serde_json::from_value(value).unwrap();
            attach(
                &mut auth.store,
                node.clone(),
                NodeState::Listed {
                    status: Status::Active,
                    maintenance: None,
                },
                alias.into(),
            )
            .unwrap();
            node
        };
        let target = add("primary", 11);
        let spare = add("spare", 12);
        let mut head = publication::current(&auth.store).unwrap();
        let now = auth.environment.now();
        head.project(&mut auth.store, &signer, now, "init").unwrap();
        Self {
            auth,
            admin,
            signer,
            target,
            spare,
        }
    }

    fn node(&self, node_key: &str) -> Node {
        let id = crate::fleet::topology::public_key(node_key).unwrap();
        load::<Node>(&self.auth.store, &key("node", &id))
            .unwrap()
            .unwrap()
    }

    fn find_entry(&self, alias: &str) -> Option<Entry> {
        publication::current(&self.auth.store)
            .unwrap()
            .payload()
            .unwrap()
            .nodes
            .into_iter()
            .find(|e| e.spec.alias == alias)
    }

    fn manage_node(&mut self, node_key: &str, action: Action) -> anyhow::Result<()> {
        let operation_id = self.node(node_key).operation_id;
        self.auth.manage(
            &self.admin,
            &Manage {
                node_key: node_key.into(),
                operation_id,
                action,
            },
            &self.signer,
        )
    }

    fn abort_target(&mut self, maintenance_id: &str) -> anyhow::Result<()> {
        let target_key = self.target.node_key.clone();
        self.manage_node(
            &target_key,
            Action::AbortMaintenance {
                maintenance_id: maintenance_id.into(),
            },
        )
    }
}

fn assert_conflict(err: &anyhow::Error, expected: &str) {
    assert!(matches!(
        err.downcast_ref::<Fault>(),
        Some(Fault::Conflict { reason }) if reason == expected
    ));
}

#[test]
fn abort_maintenance_on_listed_node_keeps_draining_and_fences_aborted_id() {
    let mut fleet = MaintenanceFleet::new();
    let aborted = Operation {
        node_key: fleet.target.node_key.clone(),
        operation_id: "aborted-upgrade-0001".into(),
    };
    let spare_op = Operation {
        node_key: fleet.spare.node_key.clone(),
        operation_id: "spare-upgrade-000001".into(),
    };

    fleet
        .auth
        .maintenance_hold(&aborted, true, &fleet.signer)
        .unwrap();
    assert_conflict(
        &fleet
            .auth
            .maintenance_hold(&spare_op, true, &fleet.signer)
            .err()
            .unwrap(),
        "maintenance_in_progress",
    );

    let events = super::observability::capture_events(|| {
        fleet.abort_target(&aborted.operation_id).unwrap();
    });
    let aborted_events: Vec<_> = events
        .iter()
        .filter(|(level, entry)| {
            *level == Level::WARN && entry["fields"]["event"] == "maintenance_hold_aborted"
        })
        .map(|(_, entry)| entry)
        .collect();
    assert_eq!(aborted_events.len(), 1);
    assert_eq!(
        aborted_events[0]["fields"]["maintenance_id"],
        aborted.operation_id
    );
    assert_eq!(
        aborted_events[0]["fields"]["node_id"],
        fleet.target.node_key
    );

    // Fail-closed: node remains draining rather than automatically returning to Active.
    let node = fleet.node(&fleet.target.node_key);
    assert!(node.maintenance().is_none());
    assert!(matches!(
        node.state(),
        NodeState::Listed {
            status: Status::Draining,
            maintenance: None,
        }
    ));
    assert_eq!(node.admission(), Admission::Draining);
    assert_eq!(
        fleet.find_entry("primary").unwrap().status,
        Status::Draining
    );

    // The aborted operation is permanently fenced off and cannot reclaim or probe.
    assert!(!fleet.auth.maintenance_status(&aborted).unwrap().held);
    let reacquire_err = fleet
        .auth
        .maintenance_hold(&aborted, true, &fleet.signer)
        .err()
        .unwrap();
    assert!(matches!(
        reacquire_err.downcast_ref::<Fault>(),
        Some(Fault::Invalid)
    ));
    let probe_err = fleet
        .auth
        .maintenance_probe(
            &aborted,
            &[[1; 12], [2; 12], [3; 12]],
            ["a", "b", "c"].map(String::from),
        )
        .err()
        .unwrap();
    assert!(matches!(
        probe_err.downcast_ref::<Fault>(),
        Some(Fault::Invalid)
    ));
    assert!(
        !fleet
            .auth
            .maintenance_hold(&aborted, false, &fleet.signer)
            .unwrap()
            .held
    );

    // Fleet-wide upgrade lock is immediately available to other nodes.
    assert!(
        fleet
            .auth
            .maintenance_hold(&spare_op, true, &fleet.signer)
            .unwrap()
            .held
    );
    assert!(
        !fleet
            .auth
            .maintenance_hold(&spare_op, false, &fleet.signer)
            .unwrap()
            .held
    );
}

#[test]
fn abort_maintenance_allows_explicit_resume_without_mirror_wait() {
    let mut fleet = MaintenanceFleet::new();
    let aborted = Operation {
        node_key: fleet.target.node_key.clone(),
        operation_id: "aborted-health-00001".into(),
    };
    fleet
        .auth
        .maintenance_hold(&aborted, true, &fleet.signer)
        .unwrap();
    fleet.abort_target(&aborted.operation_id).unwrap();

    let entry = fleet.find_entry("primary").unwrap();
    assert_eq!(entry.status, Status::Draining);
    assert_eq!(entry.spec.roles, [Role::Relay, Role::Stun]);

    // Explicit resume restores relay admission immediately without waiting for mirrors.
    let target_key = fleet.target.node_key.clone();
    fleet.manage_node(&target_key, Action::Resume).unwrap();
    assert_eq!(
        fleet.node(&fleet.target.node_key).admission(),
        Admission::Accepting
    );
    let resumed = fleet.find_entry("primary").unwrap();
    assert_eq!(resumed.status, Status::Active);
    assert_eq!(resumed.spec.roles, [Role::Relay, Role::Stun]);
}

#[test]
fn disable_is_rejected_during_maintenance_until_abort_and_disabled_cannot_hold_maintenance() {
    let mut fleet = MaintenanceFleet::new();
    let target_key = fleet.target.node_key.clone();
    let op = Operation {
        node_key: target_key.clone(),
        operation_id: "disabled-upgrade-001".into(),
    };
    fleet
        .auth
        .maintenance_hold(&op, true, &fleet.signer)
        .unwrap();
    let version_during_hold = fleet.node(&target_key).version;
    for early_cutoff in [false, true] {
        assert_conflict(
            &fleet
                .manage_node(&target_key, Action::Disable { early_cutoff })
                .unwrap_err(),
            "maintenance_in_progress",
        );
    }
    let held = fleet.node(&target_key);
    assert_eq!(held.version, version_during_hold);
    assert_eq!(held.admission(), Admission::Draining);
    assert_eq!(held.maintenance(), Some(op.operation_id.as_str()));
    assert!(fleet.find_entry("primary").is_some());

    let events = super::observability::capture_events(|| {
        fleet.abort_target(&op.operation_id).unwrap();
    });
    let aborted = super::observability::events_named(&events, "maintenance_hold_aborted");
    assert_eq!(aborted.len(), 1);
    assert_eq!(aborted[0]["fields"]["maintenance_id"], op.operation_id);

    let after_abort = fleet.node(&target_key);
    assert_eq!(after_abort.admission(), Admission::Draining);
    assert!(after_abort.maintenance().is_none());

    fleet
        .manage_node(&target_key, Action::Disable { early_cutoff: true })
        .unwrap();
    let disabled = fleet.node(&target_key);
    assert!(matches!(disabled.state(), NodeState::Disabled));
    assert_eq!(disabled.admission(), Admission::Disabled);
    assert_eq!(disabled.version, version_during_hold + 1);
    assert!(disabled.maintenance().is_none());
    assert!(fleet.find_entry("primary").is_none());

    // A disabled node cannot acquire maintenance; a stale tab attempting to abort
    // an earlier hold sees listing_changed so it refreshes cleanly.
    let late_op = Operation {
        node_key: target_key.clone(),
        operation_id: "disabled-late-hold01".into(),
    };
    let acquire_err = fleet
        .auth
        .maintenance_hold(&late_op, true, &fleet.signer)
        .err()
        .unwrap();
    assert!(matches!(
        acquire_err.downcast_ref::<Fault>(),
        Some(Fault::Invalid)
    ));
    assert_conflict(
        &fleet.abort_target(&op.operation_id).unwrap_err(),
        "listing_changed",
    );

    fleet.manage_node(&target_key, Action::Verify).unwrap();
    let staging_op = fleet.node(&target_key).operation_id;
    fleet
        .auth
        .cancel_draft(&fleet.admin, &target_key, &staging_op)
        .unwrap();
    fleet
        .manage_node(&target_key, Action::Remove { early_cutoff: true })
        .unwrap();
    let id = crate::fleet::topology::public_key(&target_key).unwrap();
    assert!(
        load::<Node>(&fleet.auth.store, &key("node", &id))
            .unwrap()
            .is_none()
    );
}

#[test]
fn abort_maintenance_rejects_invalid_ids_and_staging_nodes() {
    let mut fleet = MaintenanceFleet::new();

    for bad_id in ["", "too-short", "bad_underscore_id_1234", &"a".repeat(65)] {
        let err = fleet.abort_target(bad_id).unwrap_err();
        assert!(matches!(err.downcast_ref::<Fault>(), Some(Fault::Invalid)));
    }

    let draft_key = SigningKey::from_bytes(&[77; 32]);
    let mut draft_val = spec("staging-node", 0);
    draft_val["node_key"] = json!(URL_SAFE_NO_PAD.encode(draft_key.verifying_key().to_bytes()));
    let draft_spec: Spec = serde_json::from_value(draft_val).unwrap();
    fleet.auth.stage(&fleet.admin, draft_spec.clone()).unwrap();
    let err = fleet
        .manage_node(
            &draft_spec.node_key,
            Action::AbortMaintenance {
                maintenance_id: "staging-abort-000001".into(),
            },
        )
        .unwrap_err();
    assert!(matches!(err.downcast_ref::<Fault>(), Some(Fault::Invalid)));
}

#[test]
fn abort_maintenance_requires_admin_and_rejects_stale_maintenance_or_listing_ids() {
    let mut fleet = MaintenanceFleet::new();
    let first = Operation {
        node_key: fleet.target.node_key.clone(),
        operation_id: "first-maintenance-01".into(),
    };
    let second = Operation {
        node_key: fleet.target.node_key.clone(),
        operation_id: "second-maintenance02".into(),
    };

    // Aborting when no maintenance is held returns listing_changed without mutating Active.
    assert_conflict(
        &fleet.abort_target(&first.operation_id).unwrap_err(),
        "listing_changed",
    );
    assert_eq!(
        fleet.node(&fleet.target.node_key).admission(),
        Admission::Accepting
    );

    fleet
        .auth
        .maintenance_hold(&first, true, &fleet.signer)
        .unwrap();
    let current_op = fleet.node(&fleet.target.node_key).operation_id;
    assert!(
        fleet
            .auth
            .manage(
                "not-an-admin",
                &Manage {
                    node_key: fleet.target.node_key.clone(),
                    operation_id: current_op,
                    action: Action::AbortMaintenance {
                        maintenance_id: first.operation_id.clone(),
                    },
                },
                &fleet.signer,
            )
            .is_err()
    );
    assert_conflict(
        &fleet
            .auth
            .manage(
                &fleet.admin,
                &Manage {
                    node_key: fleet.target.node_key.clone(),
                    operation_id: "stale-listing-op".into(),
                    action: Action::AbortMaintenance {
                        maintenance_id: first.operation_id.clone(),
                    },
                },
                &fleet.signer,
            )
            .unwrap_err(),
        "listing_changed",
    );

    // A stale tab from `first` cannot abort a newer legitimate hold `second`.
    fleet
        .auth
        .maintenance_hold(&first, false, &fleet.signer)
        .unwrap();
    fleet
        .auth
        .maintenance_hold(&second, true, &fleet.signer)
        .unwrap();
    assert_conflict(
        &fleet.abort_target(&first.operation_id).unwrap_err(),
        "listing_changed",
    );
    assert_eq!(
        fleet.node(&fleet.target.node_key).maintenance(),
        Some(second.operation_id.as_str())
    );

    // Aborting `second` succeeds once; repeating it fails with listing_changed.
    fleet.abort_target(&second.operation_id).unwrap();
    assert_conflict(
        &fleet.abort_target(&second.operation_id).unwrap_err(),
        "listing_changed",
    );
}

#[test]
fn maintenance_hold_withdraws_stateless_stun_until_release() {
    let mut fleet = MaintenanceFleet::new();
    let op = Operation {
        node_key: fleet.target.node_key.clone(),
        operation_id: "stun-withdraw-hold01".into(),
    };
    fleet
        .auth
        .maintenance_hold(&op, true, &fleet.signer)
        .unwrap();

    let held = fleet.find_entry("primary").unwrap();
    assert_eq!(held.status, Status::Draining);
    assert_eq!(held.spec.roles, [Role::Relay]);
    assert_eq!(held.spec.stun, Vec::<String>::new());

    // Upgrade verification still receives the full configured STUN role/endpoints.
    let probe = fleet
        .auth
        .maintenance_probe(
            &op,
            &[[1; 12], [2; 12], [3; 12]],
            ["a", "b", "c"].map(String::from),
        )
        .unwrap();
    assert_eq!(probe.spec.roles, [Role::Relay, Role::Stun]);
    assert_eq!(probe.spec.stun, ["stun:primary.example:3478"]);

    fleet
        .auth
        .maintenance_hold(&op, false, &fleet.signer)
        .unwrap();
    let restored = fleet.find_entry("primary").unwrap();
    assert_eq!(restored.status, Status::Active);
    assert_eq!(restored.spec.roles, [Role::Relay, Role::Stun]);
    assert_eq!(restored.spec.stun, ["stun:primary.example:3478"]);

    // STUN-only nodes have no relay alias to drain and disappear while held.
    let mut stun_only = fleet.target.clone();
    stun_only.roles = vec![Role::Stun];
    attach(
        &mut fleet.auth.store,
        stun_only,
        NodeState::Listed {
            status: Status::Active,
            maintenance: None,
        },
        "stun-only-maint".into(),
    )
    .unwrap();
    let mut directory = publication::current(&fleet.auth.store).unwrap();
    let now = fleet.auth.environment.now();
    directory
        .project(&mut fleet.auth.store, &fleet.signer, now, "stun-only")
        .unwrap();
    assert_eq!(
        fleet.find_entry("primary").unwrap().spec.roles,
        [Role::Stun]
    );

    let stun_op = Operation {
        node_key: fleet.target.node_key.clone(),
        operation_id: "stun-only-hold-00001".into(),
    };
    fleet
        .auth
        .maintenance_hold(&stun_op, true, &fleet.signer)
        .unwrap();
    assert!(fleet.find_entry("primary").is_none());
    let stun_probe = fleet
        .auth
        .maintenance_probe(
            &stun_op,
            &[[1; 12], [2; 12], [3; 12]],
            ["a", "b", "c"].map(String::from),
        )
        .unwrap();
    assert_eq!(stun_probe.spec.roles, [Role::Stun]);
    assert_eq!(stun_probe.tickets, Vec::<String>::new());

    fleet
        .auth
        .maintenance_hold(&stun_op, false, &fleet.signer)
        .unwrap();
    let recovered = fleet.find_entry("primary").unwrap();
    assert_eq!(recovered.status, Status::Active);
    assert_eq!(recovered.spec.roles, [Role::Stun]);
}
