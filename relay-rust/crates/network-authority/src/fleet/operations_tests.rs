use super::*;
use crate::fleet::{
    Promote, attach,
    maintenance::Operation,
    monitoring::CYCLE_SECONDS,
    operations::{Action, Admission, Manage},
    topology::{Entry, Role},
};
use std::collections::BTreeSet;

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

    fn observe(&mut self, reports: &[(usize, [bool; 2], [bool; 2])]) {
        self.auth.environment.now += CYCLE_SECONDS;
        let at = self.auth.environment.now;
        let mirror = digest(
            &crate::fleet::publication::current(&self.auth.store)
                .unwrap()
                .envelope,
        )
        .unwrap();
        for &(index, target, spare) in reports {
            let mut nodes = serde_json::Map::new();
            for (spec, roles) in [(&self.target, target), (&self.spare, spare)] {
                let id = crate::fleet::topology::public_key(&spec.node_key).unwrap();
                let op = load::<Node>(&self.auth.store, &key("node", &id))
                    .unwrap()
                    .and_then(|n| n.listing)
                    .map_or_else(|| spec.alias.clone(), |l| l.operation_id);
                nodes.insert(
                    spec.node_key.clone(),
                    json!({
                        "operation_id": op, "tcp":true, "https":true, "tls_expires": at + 86400,
                        "relay": spec.roles.contains(&Role::Relay).then_some(roles[0]),
                        "stun": spec.roles.contains(&Role::Stun).then_some(roles[1])
                    }),
                );
            }
            let proof = proof(
                &self.keys[index],
                b"windshare/fleet-cycle/v1\0",
                &json!({"at":at,"mirror":mirror,"nodes":nodes}),
            );
            self.auth.record_cycle(&self.agents, &proof).unwrap();
            assert!(self.auth.record_cycle(&self.agents, &proof).is_err());
        }
        let mut head = crate::fleet::publication::current(&self.auth.store).unwrap();
        self.auth
            .evaluate_health(&self.agents, &self.signer, &mut head)
            .unwrap();
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
        let operation_id = self.target_node().listing.unwrap().operation_id;
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
        let operation_id = self.target_node().listing.unwrap().operation_id;
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
fn health_needs_corroboration_and_recovers_each_service_independently() {
    let mut fleet = Fleet::new();
    let ok = [true, true];
    let only_stun = [false, true];
    for _ in 0..3 {
        fleet.observe(&[(0, only_stun, ok)]);
    }
    assert_eq!(fleet.entry().status, Status::Active);
    // Cross-border disagreement is observable, but cannot change routing.
    for _ in 0..3 {
        fleet.observe(&[(0, only_stun, ok), (2, ok, ok)]);
    }
    assert_eq!(fleet.entry().status, Status::Active);
    for _ in 0..3 {
        fleet.observe(&[(0, only_stun, ok), (2, only_stun, ok)]);
    }
    assert_eq!(fleet.entry().status, Status::Draining);
    assert!(fleet.entry().spec.roles.contains(&Role::Stun));

    let only_relay = [true, false];
    for _ in 0..2 {
        fleet.observe(&[(0, only_relay, ok), (2, only_relay, ok)]);
    }
    assert_eq!(fleet.entry().status, Status::Draining);
    fleet.observe(&[(0, only_relay, ok), (2, only_relay, ok)]);
    assert_eq!(fleet.entry().status, Status::Active);
    assert_eq!(fleet.entry().spec.roles, [Role::Relay]);

    fleet
        .manage(Action::RestoreRole { role: Role::Stun })
        .unwrap();
    assert!(fleet.entry().spec.roles.contains(&Role::Stun));
    fleet.manage(Action::Drain).unwrap();
    for _ in 0..3 {
        fleet.observe(&[(0, ok, ok), (2, ok, ok)]);
    }
    assert_eq!(fleet.entry().status, Status::Draining);
}

#[test]
fn burst_and_last_active_protection_survive_sustained_faults() {
    let mut fleet = Fleet::new();
    let bad = [false, false];
    let ok = [true, true];
    for _ in 0..3 {
        fleet.observe(&[(0, bad, bad), (1, bad, bad), (2, bad, bad)]);
    }
    assert_eq!(fleet.entry().status, Status::Active);
    assert_eq!(fleet.entry().spec.roles.len(), 2);
    let remove_spare = Manage {
        node_key: fleet.spare.node_key.clone(),
        operation_id: fleet.spare.alias.clone(),
        action: Action::Disable { early_cutoff: true },
    };
    fleet
        .auth
        .manage(&fleet.admin, &remove_spare, &fleet.signer)
        .unwrap();
    for _ in 0..3 {
        fleet.observe(&[(0, bad, ok), (1, bad, ok), (2, bad, ok)]);
    }
    assert_eq!(fleet.entry().status, Status::Active);
    assert!(fleet.entry().spec.roles.contains(&Role::Stun));
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
    fleet
        .manage(Action::Disable { early_cutoff: true })
        .unwrap();
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
    assert_eq!(node.node.admission, Admission::Disabled);
    assert!(!node.cutoff_ready);
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
    assert_eq!(replacement.admission, Admission::Draining);
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
fn stun_only_node_disappears_and_recovers_without_ever_accepting_relay_traffic() {
    let mut fleet = Fleet::new();
    fleet.target.roles = vec![Role::Stun];
    let node = attach(
        &mut fleet.auth.store,
        fleet.target.clone(),
        ListingState::Listed {
            status: Status::Active,
            withdrawn: BTreeSet::new(),
        },
        fleet.target.alias.clone(),
    )
    .unwrap();
    assert_eq!(node.admission, Admission::Draining);
    for _ in 0..3 {
        fleet.observe(&[
            (0, [true, false], [true, true]),
            (1, [true, false], [true, true]),
        ]);
    }
    let directory = crate::fleet::publication::current(&fleet.auth.store)
        .unwrap()
        .payload()
        .unwrap();
    assert!(
        directory
            .nodes
            .iter()
            .all(|n| n.spec.alias != fleet.target.alias && !n.spec.roles.is_empty())
    );
    for _ in 0..3 {
        fleet.observe(&[
            (0, [true, true], [true, true]),
            (1, [true, true], [true, true]),
        ]);
    }
    assert_eq!(fleet.entry().status, Status::Active);
    assert_eq!(fleet.entry().spec.roles, [Role::Stun]);
}

#[test]
fn disable_verify_promote_clears_withdrawn_overrides_and_staging_streaks() {
    let mut fleet = Fleet::new();
    let ok = [true, true];
    let bad_relay = [false, true];
    for _ in 0..3 {
        fleet.observe(&[(0, bad_relay, ok), (1, bad_relay, ok)]);
    }
    assert_eq!(fleet.entry().status, Status::Draining);
    assert_eq!(fleet.target_node().admission, Admission::Draining);

    // Changing listed operator intent without re-verification preserves active health overrides
    // and does not wait for mirror propagation.
    fleet.manage(Action::Drain).unwrap();
    fleet.manage(Action::Resume).unwrap();
    assert_eq!(fleet.entry().status, Status::Draining);
    assert_eq!(fleet.target_node().admission, Admission::Draining);

    fleet
        .manage(Action::Disable { early_cutoff: true })
        .unwrap();
    assert!(matches!(
        fleet.target_node().listing.unwrap().state,
        ListingState::Disabled
    ));
    assert!(fleet.find_entry().is_none());
    assert!(
        fleet
            .manage(Action::RestoreRole { role: Role::Relay })
            .is_err()
    );

    fleet.manage(Action::Verify).unwrap();
    let all_bad = [false, false];
    for _ in 0..3 {
        fleet.observe(&[(0, all_bad, ok), (1, all_bad, ok)]);
    }
    fleet.pass_staging_probe();
    fleet.promote_target(Status::Active).unwrap();

    assert_eq!(fleet.target_node().admission, Admission::Accepting);
    assert_eq!(fleet.entry().status, Status::Active);
    assert_eq!(fleet.entry().spec.roles, [Role::Relay, Role::Stun]);
    assert!(matches!(
        fleet.target_node().listing.unwrap().state,
        ListingState::Listed {
            status: Status::Active,
            ref withdrawn,
        } if withdrawn.is_empty()
    ));

    // Pre-verification staging failures must not re-withdraw the freshly promoted node.
    let mut head = crate::fleet::publication::current(&fleet.auth.store).unwrap();
    fleet
        .auth
        .evaluate_health(&fleet.agents, &fleet.signer, &mut head)
        .unwrap();
    assert_eq!(fleet.target_node().admission, Admission::Accepting);
    assert_eq!(fleet.entry().status, Status::Active);

    // A STUN-only node recovers back into the signed directory after Disable -> Verify -> Promote.
    let mut fleet = Fleet::new();
    fleet.target.roles = vec![Role::Stun];
    attach(
        &mut fleet.auth.store,
        fleet.target.clone(),
        ListingState::Listed {
            status: Status::Active,
            withdrawn: BTreeSet::new(),
        },
        "stun-only".into(),
    )
    .unwrap();
    for _ in 0..3 {
        fleet.observe(&[(0, [true, false], ok), (1, [true, false], ok)]);
    }
    assert!(fleet.find_entry().is_none());

    fleet
        .manage(Action::Disable { early_cutoff: true })
        .unwrap();
    fleet.manage(Action::Verify).unwrap();
    fleet.observe(&[(0, ok, ok), (1, ok, ok)]);
    fleet.pass_staging_probe();
    fleet.promote_target(Status::Active).unwrap();

    assert_eq!(fleet.entry().status, Status::Active);
    assert_eq!(fleet.entry().spec.roles, [Role::Stun]);
    assert_eq!(fleet.target_node().admission, Admission::Draining);
}

#[test]
fn disable_during_maintenance_does_not_republish_withdrawn_stun() {
    let mut fleet = Fleet::new();
    let ok = [true, true];
    let bad_stun = [true, false];
    for _ in 0..3 {
        fleet.observe(&[(0, bad_stun, ok), (1, bad_stun, ok)]);
    }
    assert_eq!(fleet.entry().spec.roles, [Role::Relay]);
    assert_eq!(fleet.entry().spec.stun, Vec::<String>::new());

    let hold = Operation {
        node_key: fleet.target.node_key.clone(),
        operation_id: "upgrade-with-disable".into(),
    };
    fleet
        .auth
        .maintenance_hold(&hold, true, &fleet.signer)
        .unwrap();
    fleet
        .manage(Action::Disable { early_cutoff: true })
        .unwrap();
    assert_eq!(fleet.entry().status, Status::Draining);
    assert_eq!(fleet.entry().spec.roles, [Role::Relay]);
    assert_eq!(fleet.entry().spec.stun, Vec::<String>::new());

    fleet
        .auth
        .maintenance_hold(&hold, false, &fleet.signer)
        .unwrap();
    assert!(fleet.find_entry().is_none());
    assert_eq!(fleet.target_node().admission, Admission::Disabled);

    // STUN-only nodes have no relay sessions to drain and stay omitted when disabled under hold.
    let mut fleet = Fleet::new();
    fleet.target.roles = vec![Role::Stun];
    attach(
        &mut fleet.auth.store,
        fleet.target.clone(),
        ListingState::Listed {
            status: Status::Active,
            withdrawn: BTreeSet::new(),
        },
        "stun-maintenance".into(),
    )
    .unwrap();
    for _ in 0..3 {
        fleet.observe(&[(0, bad_stun, ok), (1, bad_stun, ok)]);
    }
    assert!(fleet.find_entry().is_none());

    let stun_hold = Operation {
        node_key: fleet.target.node_key.clone(),
        operation_id: "upgrade-stun-only".into(),
    };
    fleet
        .auth
        .maintenance_hold(&stun_hold, true, &fleet.signer)
        .unwrap();
    fleet
        .manage(Action::Disable { early_cutoff: true })
        .unwrap();
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
    assert!(fleet.find_entry().is_none());
    assert_eq!(fleet.target_node().admission, Admission::Disabled);
}

struct MultiNodeFleet {
    auth: Authority<Memory, Clock>,
    signer: Publisher,
    keys: [SigningKey; 3],
    agents: Vec<Agent>,
    specs: Vec<Spec>,
}

impl MultiNodeFleet {
    fn new(initial: &[(&str, u8, Zone, Status)]) -> Self {
        let mut auth = fixture();
        let (release, signer, history) = directory();
        Publication::bootstrap(&mut auth.store, &release, &[&history]).unwrap();
        // Remove the bootstrap `prior` node so the fleet size matches the test topology exactly.
        auth.store.delete(&key("node", &[9; 32])).unwrap();
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
        let mut fleet = Self {
            auth,
            signer,
            keys,
            agents,
            specs: Vec::new(),
        };
        for &(alias, byte, zone, status) in initial {
            let mut value = spec(alias, byte);
            value["node_key"] = json!(
                URL_SAFE_NO_PAD.encode(
                    SigningKey::from_bytes(&[byte; 32])
                        .verifying_key()
                        .to_bytes()
                )
            );
            value["zone"] = json!(zone);
            value["roles"] = json!(["relay", "stun"]);
            value["stun"] = json!([format!("stun:{alias}.example:3478")]);
            let node: Spec = serde_json::from_value(value).unwrap();
            attach(
                &mut fleet.auth.store,
                node.clone(),
                ListingState::Listed {
                    status,
                    withdrawn: BTreeSet::new(),
                },
                alias.into(),
            )
            .unwrap();
            fleet.specs.push(node);
        }
        let mut head = crate::fleet::publication::current(&fleet.auth.store).unwrap();
        let now = fleet.auth.environment.now();
        head.project(&mut fleet.auth.store, &fleet.signer, now, "init")
            .unwrap();
        fleet
    }

    fn observe(&mut self, per_vantage: &[&[(&str, [bool; 2])]]) {
        self.auth.environment.now += CYCLE_SECONDS;
        let at = self.auth.environment.now;
        for (vantage, reports) in per_vantage.iter().enumerate() {
            let mut nodes = serde_json::Map::new();
            for &(alias, roles) in *reports {
                let spec = self.specs.iter().find(|s| s.alias == alias).unwrap();
                nodes.insert(
                    spec.node_key.clone(),
                    json!({
                        "operation_id": spec.alias,
                        "tcp": true,
                        "https": true,
                        "tls_expires": at + 86400,
                        "relay": roles[0],
                        "stun": roles[1],
                    }),
                );
            }
            let proof = proof(
                &self.keys[vantage],
                b"windshare/fleet-cycle/v1\0",
                &json!({"at": at, "mirror": null, "nodes": nodes}),
            );
            self.auth.record_cycle(&self.agents, &proof).unwrap();
        }
        let mut head = crate::fleet::publication::current(&self.auth.store).unwrap();
        self.auth
            .evaluate_health(&self.agents, &self.signer, &mut head)
            .unwrap();
    }

    fn entry(&self, alias: &str) -> Entry {
        crate::fleet::publication::current(&self.auth.store)
            .unwrap()
            .payload()
            .unwrap()
            .nodes
            .into_iter()
            .find(|e| e.spec.alias == alias)
            .unwrap()
    }
}

#[test]
fn single_vantage_cross_border_drop_does_not_blind_small_fleet_failover() {
    let mut fleet = MultiNodeFleet::new(&[
        ("cn1", 31, Zone::Cn, Status::Active),
        ("us1", 32, Zone::Global, Status::Active),
        ("eu1", 33, Zone::Global, Status::Active),
    ]);
    let (ok, bad, cross_border_stun_drop) = ([true, true], [false, false], [true, false]);

    for cycle in 0..3 {
        let abroad_cn1 = if cycle == 2 {
            cross_border_stun_drop
        } else {
            ok
        };
        fleet.observe(&[
            &[("cn1", ok), ("us1", bad), ("eu1", ok)],
            &[("cn1", ok), ("us1", bad), ("eu1", ok)],
            &[("cn1", abroad_cn1), ("us1", bad), ("eu1", ok)],
        ]);
    }
    assert_eq!(fleet.entry("us1").status, Status::Draining);
    assert_eq!(fleet.entry("us1").spec.roles, [Role::Relay]);
    assert_eq!(fleet.entry("cn1").status, Status::Active);
    assert_eq!(fleet.entry("cn1").spec.roles, [Role::Relay, Role::Stun]);
}

#[test]
fn draining_canary_and_already_withdrawn_roles_do_not_trigger_burst_suppression() {
    let mut fleet = MultiNodeFleet::new(&[
        ("cn1", 41, Zone::Cn, Status::Active),
        ("cn2", 42, Zone::Cn, Status::Active),
        ("us1", 43, Zone::Global, Status::Active),
        ("eu1", 44, Zone::Global, Status::Active),
        ("old-us", 45, Zone::Global, Status::Draining),
        ("jp1", 46, Zone::Global, Status::Canary),
    ]);
    let (ok, bad, stun_down) = ([true, true], [false, false], [true, false]);

    // Step 1: cn1 loses STUN while old-us (Draining) and jp1 (Canary) are both down.
    for _ in 0..3 {
        let report = [
            ("cn1", stun_down),
            ("cn2", ok),
            ("us1", ok),
            ("eu1", ok),
            ("old-us", bad),
            ("jp1", bad),
        ];
        fleet.observe(&[&report, &report, &report]);
    }
    assert_eq!(fleet.entry("cn1").status, Status::Active);
    assert_eq!(fleet.entry("cn1").spec.roles, [Role::Relay]);
    assert_eq!(fleet.entry("jp1").status, Status::Draining);
    assert_eq!(fleet.entry("jp1").spec.roles, [Role::Relay]);
    assert_eq!(fleet.entry("old-us").spec.roles, [Role::Relay]);

    // Step 2: While cn1's already-withdrawn STUN keeps failing, us1 suffers a real outage.
    for _ in 0..3 {
        let report = [
            ("cn1", stun_down),
            ("cn2", ok),
            ("us1", bad),
            ("eu1", ok),
            ("old-us", bad),
            ("jp1", bad),
        ];
        fleet.observe(&[&report, &report, &report]);
    }
    assert_eq!(fleet.entry("us1").status, Status::Draining);
    assert_eq!(fleet.entry("us1").spec.roles, [Role::Relay]);

    // Step 3: After us1 has all roles withdrawn and stays down, cn1 losing Relay still fails over.
    for _ in 0..3 {
        let report = [
            ("cn1", bad),
            ("cn2", ok),
            ("us1", bad),
            ("eu1", ok),
            ("old-us", bad),
            ("jp1", bad),
        ];
        fleet.observe(&[&report, &report, &report]);
    }
    assert_eq!(fleet.entry("cn1").status, Status::Draining);
}

#[test]
fn corroborated_multi_node_burst_suppresses_withdrawal_even_with_healthy_alternative() {
    let mut fleet = MultiNodeFleet::new(&[
        ("cn1", 51, Zone::Cn, Status::Active),
        ("us1", 52, Zone::Global, Status::Active),
        ("eu1", 53, Zone::Global, Status::Active),
    ]);
    let (ok, bad) = ([true, true], [false, false]);

    for _ in 0..2 {
        let report = [("cn1", ok), ("us1", bad), ("eu1", ok)];
        fleet.observe(&[&report, &report, &report]);
    }
    // On cycle 3, us1 reaches sustained failure while cn1 also fails across all vantages.
    // eu1 stays healthy so alternative(us1) is true, isolating fleet-wide burst protection.
    let burst_report = [("cn1", bad), ("us1", bad), ("eu1", ok)];
    fleet.observe(&[&burst_report, &burst_report, &burst_report]);
    assert_eq!(fleet.entry("us1").status, Status::Active);
    assert_eq!(fleet.entry("us1").spec.roles, [Role::Relay, Role::Stun]);

    // Once cn1 recovers, burst clears and us1 fails over to eu1 on the next cycle.
    let single_fault = [("cn1", ok), ("us1", bad), ("eu1", ok)];
    fleet.observe(&[&single_fault, &single_fault, &single_fault]);
    assert_eq!(fleet.entry("us1").status, Status::Draining);
    assert_eq!(fleet.entry("us1").spec.roles, [Role::Relay]);
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
    let provisional_op = provisional.listing.as_ref().unwrap().operation_id.clone();
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

    // PriorPolicy draft rejects Disable so cancelling still restores its prior Admission.
    let prior_key = SigningKey::from_bytes(&[62; 32]);
    let prior_id = prior_key.verifying_key().to_bytes();
    let prior_node = Node {
        listing: None,
        key: prior_id,
        zone: Zone::Cn,
        region: "shanghai".into(),
        admission: Admission::Accepting,
        anonymous: false,
        version: 4,
    };
    save(&mut fleet.auth.store, &key("node", &prior_id), &prior_node).unwrap();
    let prior_spec = spec_for_key("prior-policy-draft", &prior_key);
    let staged_prior = fleet.auth.stage(&fleet.admin, prior_spec.clone()).unwrap();
    let prior_op = staged_prior.listing.as_ref().unwrap().operation_id.clone();
    report_zero_shares(&mut fleet.auth, &prior_key, staged_prior.version);
    assert!(
        fleet
            .auth
            .node_by_key(&fleet.admin, &prior_id)
            .unwrap()
            .cutoff_ready
    );
    assert_staging_disable_rejected(&mut fleet, &prior_spec.node_key, &prior_op);
    fleet
        .auth
        .cancel_draft(&fleet.admin, &prior_spec.node_key, &prior_op)
        .unwrap();
    let restored: Node = load(&fleet.auth.store, &key("node", &prior_id))
        .unwrap()
        .unwrap();
    assert!(restored.listing.is_none());
    assert_eq!(restored.admission, Admission::Accepting);
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
    let verify_op = verify_node.listing.unwrap().operation_id;
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
    assert!(matches!(
        fleet.target_node().listing.unwrap().state,
        ListingState::Disabled
    ));
}
