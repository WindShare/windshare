use super::*;
use crate::fleet::{
    attach,
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
                ListingState::Listed {
                    status: Status::Active,
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
        for &(index, target, spare) in reports {
            let mut nodes = serde_json::Map::new();
            for (spec, roles) in [(&self.target, target), (&self.spare, spare)] {
                nodes.insert(spec.node_key.clone(), json!({
                    "operation_id": spec.alias, "tcp":true, "https":true, "tls_expires": at + 86400,
                    "relay": spec.roles.contains(&Role::Relay).then_some(roles[0]),
                    "stun": spec.roles.contains(&Role::Stun).then_some(roles[1])
                }));
            }
            let proof = proof(
                &self.keys[index],
                b"windshare/fleet-cycle/v1\0",
                &json!({"cycle":at/CYCLE_SECONDS,"at":at,"mirror":null,"nodes":nodes}),
            );
            self.auth.record_cycle(&self.agents, &proof).unwrap();
            assert!(self.auth.record_cycle(&self.agents, &proof).is_err());
        }
        let mut head = crate::fleet::publication::current(&self.auth.store).unwrap();
        self.auth
            .evaluate_health(&self.agents, &self.signer, &mut head)
            .unwrap();
    }

    fn entry(&self) -> Entry {
        crate::fleet::publication::current(&self.auth.store)
            .unwrap()
            .payload()
            .unwrap()
            .nodes
            .into_iter()
            .find(|e| e.spec.alias == self.target.alias)
            .unwrap()
    }
    fn manage(&mut self, action: Action) -> anyhow::Result<()> {
        self.auth.manage(
            &self.admin,
            &Manage {
                node_key: self.target.node_key.clone(),
                operation_id: self.target.alias.clone(),
                action,
            },
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
