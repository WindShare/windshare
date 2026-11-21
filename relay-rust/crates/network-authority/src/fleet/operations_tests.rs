use super::*;
use crate::fleet::{
    Promote, attach,
    maintenance::Operation,
    monitoring::CYCLE_SECONDS,
    operations::{Action, Admission, Manage},
    topology::{Entry, Role},
};

struct Fleet {
    auth: Authority<Memory, Clock>,
    admin: String,
    signer: Publisher,
    keys: [SigningKey; 3],
    agents: Vec<Agent>,
    target: Spec,
    spare: Spec,
}

impl Fleet {
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
        let mut head = crate::fleet::publication::current(&auth.store).unwrap();
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

    fn report_current_mirrors(&mut self) {
        self.auth.environment.now += CYCLE_SECONDS;
        let at = self.auth.environment.now;
        let mirror = digest(
            &crate::fleet::publication::current(&self.auth.store)
                .unwrap()
                .envelope,
        )
        .unwrap();
        for index in [0, 1] {
            let report = proof(
                &self.keys[index],
                b"windshare/fleet-cycle/v1\0",
                &json!({"at": at, "mirror": mirror, "nodes": {}}),
            );
            self.auth.record_cycle(&self.agents, &report).unwrap();
            assert!(self.auth.record_cycle(&self.agents, &report).is_err());
        }
    }

    fn target_node(&self) -> Node {
        let id = crate::fleet::topology::public_key(&self.target.node_key).unwrap();
        load::<Node>(&self.auth.store, &key("node", &id))
            .unwrap()
            .unwrap()
    }

    fn find_entry(&self) -> Option<Entry> {
        crate::fleet::publication::current(&self.auth.store)
            .unwrap()
            .payload()
            .unwrap()
            .nodes
            .into_iter()
            .find(|e| e.spec.alias == self.target.alias)
    }

    fn entry(&self) -> Entry {
        self.find_entry().unwrap()
    }

    fn manage(&mut self, action: Action) -> anyhow::Result<()> {
        let operation_id = self.target_node().operation_id;
        self.auth.manage(
            &self.admin,
            &Manage {
                node_key: self.target.node_key.clone(),
                operation_id,
                action,
            },
            &self.signer,
        )
    }

    fn pass_staging_probe(&mut self) {
        self.auth.environment.now += 1;
        let at = self.auth.environment.now;
        let shares = [[1u8; 12], [2u8; 12], [3u8; 12]];
        let poll = proof(
            &self.keys[0],
            b"windshare/vantage-poll/v1\0",
            &json!({"at": at, "shares": shares}),
        );
        let work = self
            .auth
            .claim(&self.agents, &poll, Default::default())
            .unwrap()
            .unwrap();
        let outcome = proof(
            &self.keys[0],
            b"windshare/vantage-result/v1\0",
            &json!({"at": at, "node_key": self.target.node_key, "lease": work.lease, "outcome": {"kind": "passed"}}),
        );
        self.auth.complete_probe(&self.agents, &outcome).unwrap();
    }

    fn promote_target(&mut self, status: Status) -> anyhow::Result<()> {
        let operation_id = self.target_node().operation_id;
        let observed = digest(
            &crate::fleet::publication::current(&self.auth.store)
                .unwrap()
                .envelope,
        )
        .unwrap();
        self.auth.promote(
            &self.admin,
            &Promote {
                node_key: self.target.node_key.clone(),
                operation_id,
                status,
            },
            &self.agents,
            &observed,
            &self.signer,
        )
    }
}

#[test]
fn maintenance_serializes_the_fleet_and_releases_to_current_intent() {
    let mut fleet = Fleet::new();
    let op = Operation {
        node_key: fleet.target.node_key.clone(),
        operation_id: "first-maintenance".into(),
    };
    let other = Operation {
        node_key: fleet.spare.node_key.clone(),
        operation_id: "second-maintenance".into(),
    };
    let stranger = Operation {
        node_key: op.node_key.clone(),
        operation_id: other.operation_id.clone(),
    };
    let drain = fleet
        .auth
        .maintenance_hold(&op, true, &fleet.signer)
        .unwrap();
    assert!(drain.held && !drain.confirmed);
    assert!(
        fleet
            .auth
            .maintenance_hold(&other, true, &fleet.signer)
            .is_err()
    );
    assert!(
        fleet
            .auth
            .maintenance_hold(&stranger, false, &fleet.signer)
            .is_err()
    );
    assert_eq!(
        fleet
            .auth
            .maintenance_hold(&op, true, &fleet.signer)
            .unwrap()
            .policy_version,
        drain.policy_version
    );
    for early_cutoff in [false, true] {
        let err = fleet.manage(Action::Disable { early_cutoff }).unwrap_err();
        assert!(matches!(
            err.downcast_ref::<Fault>(),
            Some(Fault::Conflict { reason }) if reason == "maintenance_in_progress"
        ));
    }
    fleet.manage(Action::Drain).unwrap();
    let evidence = fleet
        .auth
        .maintenance_probe(
            &op,
            &[[1; 12], [2; 12], [3; 12]],
            ["a", "b", "c"].map(String::from),
        )
        .unwrap();
    assert_eq!(evidence.tickets.len(), 3);
    assert_eq!(fleet.entry().status, Status::Draining);

    let released = fleet
        .auth
        .maintenance_hold(&op, false, &fleet.signer)
        .unwrap();
    assert!(!released.held);
    let node = fleet
        .auth
        .node_by_key(
            &fleet.admin,
            &SigningKey::from_bytes(&[11; 32]).verifying_key().to_bytes(),
        )
        .unwrap();
    assert_eq!(node.admission, Admission::Draining);
    assert_eq!(fleet.entry().status, Status::Draining);

    fleet
        .manage(Action::Disable { early_cutoff: true })
        .unwrap();
    let disabled = fleet
        .auth
        .node_by_key(
            &fleet.admin,
            &SigningKey::from_bytes(&[11; 32]).verifying_key().to_bytes(),
        )
        .unwrap();
    assert_eq!(disabled.admission, Admission::Disabled);
    assert!(!disabled.cutoff_ready);
    assert!(
        fleet
            .manage(Action::Remove {
                early_cutoff: false
            })
            .is_err()
    );
}

#[test]
fn normal_removal_requires_fresh_signed_zero_and_offline_nodes_receive_the_final_policy() {
    let mut fleet = Fleet::new();
    let node = SigningKey::from_bytes(&[11; 32]);
    let key = node.verifying_key().to_bytes();
    let status = |auth: &mut Authority<Memory, Clock>, version, residents| {
        auth.environment.now += 1;
        let b = Batch {
            id: [42; 16],
            node: key,
            issuer: auth.signing.generation.issuer,
            created_at: auth.environment.now,
            records: vec![],
            resident_accounts: vec![],
            shares: vec![],
            load: Load {
                applied_policy_version: version,
                resident_shares: residents,
                relay_budget: None,
                active_sessions: 0,
                wire_bytes: 0,
                forwarded_bytes: 0,
                admission_refusals: 0,
            },
        };
        let signed = trust::sign(USAGE_DOMAIN, &serde_json::to_vec(&b).unwrap(), &node);
        let ReportResult::Status { policy, .. } = reply(auth, &signed) else {
            panic!("expected signed policy");
        };
        policy
    };
    fleet.manage(Action::Drain).unwrap();
    let drain = status(&mut fleet.auth, 0, 0);
    let disable = || Action::Disable {
        early_cutoff: false,
    };
    let remove = || Action::Remove {
        early_cutoff: false,
    };
    assert!(fleet.manage(disable()).is_err());
    status(&mut fleet.auth, drain.version, 1);
    assert!(fleet.manage(disable()).is_err());
    status(&mut fleet.auth, drain.version, 0);
    fleet.auth.environment.now += 2 * windshare_relay_access::usage::REPORT_INTERVAL_SECONDS + 1;
    assert!(fleet.manage(disable()).is_err());
    status(&mut fleet.auth, drain.version, 0);
    fleet.manage(disable()).unwrap();
    assert!(fleet.manage(remove()).is_err());
    let disabled = status(&mut fleet.auth, drain.version, 0);
    assert_eq!(disabled.admission, Admission::Disabled);
    status(&mut fleet.auth, disabled.version, 0);
    fleet.manage(remove()).unwrap();
    assert_eq!(status(&mut fleet.auth, 0, 0), disabled);
    let replacement = fleet.auth.stage(&fleet.admin, fleet.target).unwrap();
    assert!(replacement.version > disabled.version);
    assert_eq!(replacement.admission(), Admission::Draining);
}

#[test]
fn release_before_a_delayed_acquire_cannot_leave_a_drain_behind() {
    let mut fleet = Fleet::new();
    let op = Operation {
        node_key: fleet.target.node_key.clone(),
        operation_id: "cancel-before-drain".into(),
    };
    assert!(
        !fleet
            .auth
            .maintenance_hold(&op, false, &fleet.signer)
            .unwrap()
            .held
    );
    assert!(
        fleet
            .auth
            .maintenance_hold(&op, true, &fleet.signer)
            .is_err()
    );
}

#[test]
fn disable_verify_promote_republishes_dual_role_and_stun_only_nodes() {
    let mut fleet = Fleet::new();
    fleet.manage(Action::Drain).unwrap();
    assert_eq!(fleet.entry().status, Status::Draining);
    assert_eq!(fleet.target_node().admission(), Admission::Draining);

    fleet.manage(Action::Resume).unwrap();
    assert_eq!(fleet.entry().status, Status::Active);
    assert_eq!(fleet.target_node().admission(), Admission::Accepting);

    fleet
        .manage(Action::Disable { early_cutoff: true })
        .unwrap();
    assert!(matches!(fleet.target_node().state(), NodeState::Disabled));
    assert!(fleet.find_entry().is_none());

    fleet.manage(Action::Verify).unwrap();
    fleet.report_current_mirrors();
    fleet.pass_staging_probe();
    fleet.promote_target(Status::Active).unwrap();

    assert_eq!(fleet.target_node().admission(), Admission::Accepting);
    assert_eq!(fleet.entry().status, Status::Active);
    assert_eq!(fleet.entry().spec.roles, [Role::Relay, Role::Stun]);
    assert!(matches!(
        fleet.target_node().state(),
        NodeState::Listed {
            status: Status::Active,
            maintenance: None,
        }
    ));

    // A STUN-only node stays relay-draining and republishes cleanly after Disable -> Verify -> Promote.
    let mut fleet = Fleet::new();
    fleet.target.roles = vec![Role::Stun];
    let node = attach(
        &mut fleet.auth.store,
        fleet.target.clone(),
        NodeState::Listed {
            status: Status::Active,
            maintenance: None,
        },
        "stun-only".into(),
    )
    .unwrap();
    assert_eq!(node.admission(), Admission::Draining);

    fleet
        .manage(Action::Disable { early_cutoff: true })
        .unwrap();
    assert!(fleet.find_entry().is_none());
    fleet.manage(Action::Verify).unwrap();
    fleet.report_current_mirrors();
    fleet.pass_staging_probe();
    fleet.promote_target(Status::Active).unwrap();

    assert_eq!(fleet.entry().status, Status::Active);
    assert_eq!(fleet.entry().spec.roles, [Role::Stun]);
    assert_eq!(fleet.target_node().admission(), Admission::Draining);
}

#[test]
fn rejected_disable_during_maintenance_preserves_maintenance_drain_and_stun_only_probes() {
    let mut fleet = Fleet::new();
    let hold = Operation {
        node_key: fleet.target.node_key.clone(),
        operation_id: "upgrade-with-disable".into(),
    };
    fleet
        .auth
        .maintenance_hold(&hold, true, &fleet.signer)
        .unwrap();
    let err = fleet
        .manage(Action::Disable { early_cutoff: true })
        .unwrap_err();
    assert!(matches!(
        err.downcast_ref::<Fault>(),
        Some(Fault::Conflict { reason }) if reason == "maintenance_in_progress"
    ));
    assert_eq!(fleet.entry().status, Status::Draining);
    assert_eq!(fleet.entry().spec.roles, [Role::Relay]);
    assert_eq!(fleet.entry().spec.stun, Vec::<String>::new());

    fleet
        .auth
        .maintenance_hold(&hold, false, &fleet.signer)
        .unwrap();
    assert_eq!(fleet.entry().status, Status::Active);
    assert_eq!(fleet.entry().spec.roles, [Role::Relay, Role::Stun]);
    assert_eq!(fleet.entry().spec.stun, ["stun:primary.example:3478"]);

    fleet
        .manage(Action::Disable { early_cutoff: true })
        .unwrap();
    assert!(fleet.find_entry().is_none());
    assert_eq!(fleet.target_node().admission(), Admission::Disabled);

    // STUN-only nodes stay omitted under hold, reject disable, and still allow maintenance probes.
    let mut fleet = Fleet::new();
    fleet.target.roles = vec![Role::Stun];
    attach(
        &mut fleet.auth.store,
        fleet.target.clone(),
        NodeState::Listed {
            status: Status::Active,
            maintenance: None,
        },
        "stun-maintenance".into(),
    )
    .unwrap();

    let stun_hold = Operation {
        node_key: fleet.target.node_key.clone(),
        operation_id: "upgrade-stun-only".into(),
    };
    fleet
        .auth
        .maintenance_hold(&stun_hold, true, &fleet.signer)
        .unwrap();
    let stun_err = fleet
        .manage(Action::Disable { early_cutoff: true })
        .unwrap_err();
    assert!(matches!(
        stun_err.downcast_ref::<Fault>(),
        Some(Fault::Conflict { reason }) if reason == "maintenance_in_progress"
    ));
    assert!(fleet.find_entry().is_none());
    assert_eq!(
        fleet
            .auth
            .maintenance_probe(
                &stun_hold,
                &[[1; 12], [2; 12], [3; 12]],
                ["a", "b", "c"].map(String::from),
            )
            .unwrap()
            .tickets,
        Vec::<String>::new()
    );

    fleet
        .auth
        .maintenance_hold(&stun_hold, false, &fleet.signer)
        .unwrap();
    fleet
        .manage(Action::Disable { early_cutoff: true })
        .unwrap();
    assert!(fleet.find_entry().is_none());
    assert_eq!(fleet.target_node().admission(), Admission::Disabled);
}

fn report_zero_shares(auth: &mut Authority<Memory, Clock>, signing: &SigningKey, version: u64) {
    auth.environment.now += 1;
    let batch = Batch {
        id: [7; 16],
        node: signing.verifying_key().to_bytes(),
        issuer: auth.signing.generation.issuer,
        created_at: auth.environment.now,
        records: vec![],
        resident_accounts: vec![],
        shares: vec![],
        load: Load {
            applied_policy_version: version,
            resident_shares: 0,
            relay_budget: None,
            active_sessions: 0,
            wire_bytes: 0,
            forwarded_bytes: 0,
            admission_refusals: 0,
        },
    };
    let signed = trust::sign(USAGE_DOMAIN, &serde_json::to_vec(&batch).unwrap(), signing);
    assert!(matches!(reply(auth, &signed), ReportResult::Status { .. }));
}

fn assert_staging_disable_rejected(fleet: &mut Fleet, node_key: &str, operation_id: &str) {
    for early_cutoff in [false, true] {
        let err = fleet
            .auth
            .manage(
                &fleet.admin,
                &Manage {
                    node_key: node_key.into(),
                    operation_id: operation_id.into(),
                    action: Action::Disable { early_cutoff },
                },
                &fleet.signer,
            )
            .unwrap_err();
        assert!(matches!(err.downcast_ref::<Fault>(), Some(Fault::Invalid)));
    }
}

fn spec_for_key(alias: &str, signing: &SigningKey) -> Spec {
    let mut value = spec(alias, 0);
    value["node_key"] = json!(URL_SAFE_NO_PAD.encode(signing.verifying_key().to_bytes()));
    serde_json::from_value(value).unwrap()
}

#[test]
fn disable_rejects_staging_drafts_without_discarding_enrollment() {
    let mut fleet = Fleet::new();

    // Provisional draft stays staging and cancellable even after reporting zero shares.
    let provisional_key = SigningKey::from_bytes(&[61; 32]);
    let provisional_spec = spec_for_key("provisional-draft", &provisional_key);
    let provisional = fleet
        .auth
        .stage(&fleet.admin, provisional_spec.clone())
        .unwrap();
    let provisional_op = provisional.operation_id;
    report_zero_shares(&mut fleet.auth, &provisional_key, provisional.version);
    assert!(
        fleet
            .auth
            .node_by_key(&fleet.admin, &provisional.key)
            .unwrap()
            .cutoff_ready
    );
    assert_staging_disable_rejected(&mut fleet, &provisional_spec.node_key, &provisional_op);
    fleet
        .auth
        .cancel_draft(&fleet.admin, &provisional_spec.node_key, &provisional_op)
        .unwrap();
    assert!(
        load::<Node>(&fleet.auth.store, &key("node", &provisional.key))
            .unwrap()
            .is_none()
    );
}

#[test]
fn disable_is_idempotent_when_disabled_and_rejects_reverification_staging() {
    let mut fleet = Fleet::new();
    fleet
        .manage(Action::Disable { early_cutoff: true })
        .unwrap();
    let disabled_version = fleet.target_node().version;
    let disabled_serial = crate::fleet::publication::current(&fleet.auth.store)
        .unwrap()
        .revision()
        .unwrap()
        .serial;
    let target_id = fleet.target_node().key;
    assert!(
        !fleet
            .auth
            .node_by_key(&fleet.admin, &target_id)
            .unwrap()
            .cutoff_ready
    );
    for early_cutoff in [false, true] {
        fleet.manage(Action::Disable { early_cutoff }).unwrap();
    }
    assert_eq!(fleet.target_node().version, disabled_version);
    assert_eq!(
        crate::fleet::publication::current(&fleet.auth.store)
            .unwrap()
            .revision()
            .unwrap()
            .serial,
        disabled_serial
    );

    fleet.manage(Action::Verify).unwrap();
    let verify_node = fleet.target_node();
    let verify_op = verify_node.operation_id;
    let target_signing = SigningKey::from_bytes(&[11; 32]);
    report_zero_shares(&mut fleet.auth, &target_signing, verify_node.version);
    assert!(
        fleet
            .auth
            .node_by_key(&fleet.admin, &target_id)
            .unwrap()
            .cutoff_ready
    );
    let target_key = fleet.target.node_key.clone();
    assert_staging_disable_rejected(&mut fleet, &target_key, &verify_op);
    fleet
        .auth
        .cancel_draft(&fleet.admin, &target_key, &verify_op)
        .unwrap();
    assert!(matches!(fleet.target_node().state(), NodeState::Disabled));
}
