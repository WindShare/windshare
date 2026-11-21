use super::*;
use crate::fleet::{
    Promote, attach,
    maintenance::Operation,
    monitoring::CYCLE_SECONDS,
    operations::{Action, Admission, Manage},
    publication,
    topology::{Entry, Role},
};
use std::collections::BTreeSet;
use tracing::Level;

struct MaintenanceFleet {
    auth: Authority<Memory, Clock>,
    admin: String,
    signer: Publisher,
    keys: [SigningKey; 3],
    agents: Vec<Agent>,
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
                ListingState::Listed {
                    status: Status::Active,
                    withdrawn: BTreeSet::new(),
                },
                alias.into(),
            )
            .unwrap();
            node
        };
        let target = add("primary", 11);
        let spare = add("spare", 12);
        let keys = [21, 22, 23].map(|v| SigningKey::from_bytes(&[v; 32]));
        let agents = keys
            .iter()
            .zip([
                ("cn-a", Zone::Cn),
                ("cn-b", Zone::Cn),
                ("abroad", Zone::Global),
            ])
            .map(|(key, (id, zone))| Agent {
                id: id.into(),
                zone,
                key: URL_SAFE_NO_PAD.encode(key.verifying_key().to_bytes()),
            })
            .collect();
        let mut head = publication::current(&auth.store).unwrap();
        let now = auth.environment.now();
        head.project(&mut auth.store, &signer, now, "init").unwrap();
        Self {
            auth,
            admin,
            signer,
            keys,
            agents,
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
        let operation_id = self.node(node_key).listing.unwrap().operation_id;
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

    fn observe(&mut self, target_roles: [bool; 2], spare_roles: [bool; 2]) {
        self.auth.environment.now += CYCLE_SECONDS;
        let at = self.auth.environment.now;
        let mirror = digest(&publication::current(&self.auth.store).unwrap().envelope).unwrap();
        for index in [0, 1] {
            let mut nodes = serde_json::Map::new();
            for (spec, roles) in [(&self.target, target_roles), (&self.spare, spare_roles)] {
                let op = self.node(&spec.node_key).listing.unwrap().operation_id;
                nodes.insert(
                    spec.node_key.clone(),
                    json!({
                        "operation_id": op,
                        "tcp": true,
                        "https": true,
                        "tls_expires": at + 86400,
                        "relay": roles[0],
                        "stun": roles[1],
                    }),
                );
            }
            let proof = proof(
                &self.keys[index],
                b"windshare/fleet-cycle/v1\0",
                &json!({"at": at, "mirror": mirror, "nodes": nodes}),
            );
            self.auth.record_cycle(&self.agents, &proof).unwrap();
        }
        let mut head = publication::current(&self.auth.store).unwrap();
        self.auth
            .evaluate_health(&self.agents, &self.signer, &mut head)
            .unwrap();
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
    assert_eq!(aborted_events[0]["fields"]["disabled"], false);

    // Fail-closed: node remains draining rather than automatically returning to Active.
    let node = fleet.node(&fleet.target.node_key);
    let listing = node.listing.unwrap();
    assert!(listing.maintenance.is_none());
    assert!(matches!(
        listing.state,
        ListingState::Listed {
            status: Status::Draining,
            ref withdrawn,
        } if withdrawn.is_empty()
    ));
    assert_eq!(node.admission, Admission::Draining);
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
fn abort_maintenance_resumes_health_checks_and_allows_explicit_promotion() {
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

    // Health evaluation resumes once maintenance is cleared.
    for _ in 0..3 {
        fleet.observe([true, false], [true, true]);
    }
    let entry = fleet.find_entry("primary").unwrap();
    assert_eq!(entry.status, Status::Draining);
    assert_eq!(entry.spec.roles, [Role::Relay]);

    // Report the updated directory digest after STUN withdrawal so mainland mirrors match.
    fleet.observe([true, false], [true, true]);

    // Explicit promotion restores relay admission while keeping STUN withdrawn.
    let observed = digest(&publication::current(&fleet.auth.store).unwrap().envelope).unwrap();
    let op_id = fleet
        .node(&fleet.target.node_key)
        .listing
        .unwrap()
        .operation_id;
    fleet
        .auth
        .promote(
            &fleet.admin,
            &Promote {
                node_key: fleet.target.node_key.clone(),
                operation_id: op_id,
                status: Status::Active,
            },
            &fleet.agents,
            &observed,
            &fleet.signer,
        )
        .unwrap();
    assert_eq!(
        fleet.node(&fleet.target.node_key).admission,
        Admission::Accepting
    );
    let promoted = fleet.find_entry("primary").unwrap();
    assert_eq!(promoted.status, Status::Active);
    assert_eq!(promoted.spec.roles, [Role::Relay]);
}

#[test]
fn abort_maintenance_on_disabled_node_completes_disable_and_unlocks_verify_and_remove() {
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
    fleet
        .manage_node(&target_key, Action::Disable { early_cutoff: true })
        .unwrap();
    let version_during_hold = fleet.node(&target_key).version;
    assert_eq!(fleet.node(&target_key).admission, Admission::Draining);
    assert!(fleet.find_entry("primary").is_some());
    assert!(
        fleet
            .manage_node(&target_key, Action::Remove { early_cutoff: true })
            .is_err()
    );
    assert!(fleet.manage_node(&target_key, Action::Verify).is_err());

    let events = super::observability::capture_events(|| {
        fleet.abort_target(&op.operation_id).unwrap();
    });
    let aborted = super::observability::events_named(&events, "maintenance_hold_aborted");
    assert_eq!(aborted.len(), 1);
    assert_eq!(aborted[0]["fields"]["disabled"], true);

    let node = fleet.node(&target_key);
    assert_eq!(node.admission, Admission::Disabled);
    assert_eq!(node.version, version_during_hold + 1);
    assert!(node.listing.unwrap().maintenance.is_none());
    assert!(fleet.find_entry("primary").is_none());

    fleet.manage_node(&target_key, Action::Verify).unwrap();
    let staging_op = fleet.node(&target_key).listing.unwrap().operation_id;
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
        fleet.node(&fleet.target.node_key).admission,
        Admission::Accepting
    );

    fleet
        .auth
        .maintenance_hold(&first, true, &fleet.signer)
        .unwrap();
    let current_op = fleet
        .node(&fleet.target.node_key)
        .listing
        .unwrap()
        .operation_id;
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
        fleet
            .node(&fleet.target.node_key)
            .listing
            .unwrap()
            .maintenance
            .as_deref(),
        Some(second.operation_id.as_str())
    );

    // Aborting `second` succeeds once; repeating it fails with listing_changed.
    fleet.abort_target(&second.operation_id).unwrap();
    assert_conflict(
        &fleet.abort_target(&second.operation_id).unwrap_err(),
        "listing_changed",
    );
}
