use super::*;
use crate::fleet::{
    Promote, attach,
    monitoring::{CYCLE_SECONDS, MIN_CYCLE_SECONDS},
    operations::{Action, Admission, Manage},
    topology::Role,
    vantage::SILENT_SECONDS,
};
use std::collections::BTreeSet;

struct MonitoringFleet {
    auth: Authority<Memory, Clock>,
    admin: String,
    signer: Publisher,
    keys: [SigningKey; 3],
    agents: Vec<Agent>,
    active: Spec,
    disabled: Spec,
    staged: Spec,
    staged_op: String,
}

impl MonitoringFleet {
    fn new() -> Self {
        let mut auth = fixture();
        let admin = auth
            .initialize("init", "init", "operator", "reasonable password")
            .unwrap();
        let (release, signer, history) = directory();
        Publication::bootstrap(&mut auth.store, &release, &[&history]).unwrap();
        let dual_role = |alias: &str, byte: u8| -> Spec {
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
            serde_json::from_value(value).unwrap()
        };
        let active = dual_role("active-node", 11);
        let disabled = dual_role("disabled-node", 12);
        for node in [&active, &disabled] {
            attach(
                &mut auth.store,
                node.clone(),
                ListingState::Listed {
                    status: Status::Active,
                    withdrawn: BTreeSet::new(),
                },
                node.alias.clone(),
            )
            .unwrap();
        }
        auth.manage(
            &admin,
            &Manage {
                node_key: disabled.node_key.clone(),
                operation_id: disabled.alias.clone(),
                action: Action::Disable { early_cutoff: true },
            },
            &signer,
        )
        .unwrap();
        let staged = dual_role("staged-node", 13);
        let staged_op = auth
            .stage(&admin, staged.clone())
            .unwrap()
            .listing
            .unwrap()
            .operation_id;
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
            active,
            disabled,
            staged,
            staged_op,
        }
    }

    fn head_digest(&self) -> String {
        digest(
            &crate::fleet::publication::current(&self.auth.store)
                .unwrap()
                .envelope,
        )
        .unwrap()
    }

    fn target_keys(&self, vantage: usize) -> Vec<String> {
        let origins = [
            "https://ws.ourworld.top".into(),
            "https://windshare.cc".into(),
            "https://other.example".into(),
        ];
        let req = proof(
            &self.keys[vantage],
            b"windshare/fleet-targets/v1\0",
            &json!({"at": self.auth.environment.now()}),
        );
        self.auth
            .fleet_targets(&self.agents, &req, origins)
            .unwrap()
            .nodes
            .into_iter()
            .map(|target| target.spec.node_key)
            .collect()
    }

    fn verify_and_promote_staged(&mut self, status: Status, observed_digest: &str) {
        let now = self.auth.environment.now;
        let shares = [[1u8; 12], [2u8; 12], [3u8; 12]];
        let poll = proof(
            &self.keys[0],
            b"windshare/vantage-poll/v1\0",
            &json!({"at": now, "shares": shares}),
        );
        let work = self
            .auth
            .claim(&self.agents, &poll, Default::default())
            .unwrap()
            .unwrap();
        let passed = proof(
            &self.keys[0],
            b"windshare/vantage-result/v1\0",
            &json!({"at": now, "node_key": self.staged.node_key, "lease": work.lease, "outcome": {"kind": "passed"}}),
        );
        self.auth.complete_probe(&self.agents, &passed).unwrap();
        self.auth
            .promote(
                &self.admin,
                &Promote {
                    node_key: self.staged.node_key.clone(),
                    operation_id: self.staged_op.clone(),
                    status,
                },
                &self.agents,
                observed_digest,
                &self.signer,
            )
            .unwrap();
    }
    fn active_status(&self) -> Status {
        crate::fleet::publication::current(&self.auth.store)
            .unwrap()
            .payload()
            .unwrap()
            .nodes
            .into_iter()
            .find(|entry| entry.spec.node_key == self.active.node_key)
            .unwrap()
            .status
    }

    fn active_report(&self, vantage: usize, reported_at: u64, active_roles: [bool; 2]) -> Proof {
        let prior_key = URL_SAFE_NO_PAD.encode([9u8; 32]);
        let prior_op = load::<Node>(&self.auth.store, &key("node", &[9u8; 32]))
            .unwrap()
            .unwrap()
            .listing
            .unwrap()
            .operation_id;
        let nodes = json!({
            self.active.node_key.clone(): {
                "operation_id": self.active.alias, "tcp": true, "https": true,
                "tls_expires": reported_at + 86400,
                "relay": active_roles[0], "stun": active_roles[1]
            },
            prior_key: {
                "operation_id": prior_op, "tcp": true, "https": true,
                "tls_expires": reported_at + 86400,
                "relay": true, "stun": null
            },
        });
        proof(
            &self.keys[vantage],
            b"windshare/fleet-cycle/v1\0",
            &json!({"at": reported_at, "mirror": self.head_digest(), "nodes": nodes}),
        )
    }

    fn observe_active_after(&mut self, delta: u64, vantages: &[usize], active_roles: [bool; 2]) {
        self.auth.environment.now += delta;
        let at = self.auth.environment.now;
        for &vantage in vantages {
            let report = self.active_report(vantage, at, active_roles);
            self.auth.record_cycle(&self.agents, &report).unwrap();
            assert!(self.auth.record_cycle(&self.agents, &report).is_err());
        }
        let mut head = crate::fleet::publication::current(&self.auth.store).unwrap();
        self.auth
            .evaluate_health(&self.agents, &self.signer, &mut head)
            .unwrap();
    }
}

#[test]
fn fleet_targets_include_only_listed_nodes() {
    let mut fleet = MonitoringFleet::new();
    let listed = fleet.target_keys(0);
    assert_eq!(listed.len(), 2);
    assert!(
        listed.contains(&fleet.active.node_key)
            && listed.contains(&URL_SAFE_NO_PAD.encode([9u8; 32]))
    );
    assert!(!listed.contains(&fleet.disabled.node_key) && !listed.contains(&fleet.staged.node_key));

    // Listed canary and draining nodes remain monitored while disabled nodes stay excluded.
    let observed = fleet.head_digest();
    fleet.auth.environment.now += CYCLE_SECONDS;
    let at = fleet.auth.environment.now;
    let heartbeat = proof(
        &fleet.keys[0],
        b"windshare/fleet-cycle/v1\0",
        &json!({"at": at, "mirror": observed, "nodes": {}}),
    );
    fleet.auth.record_cycle(&fleet.agents, &heartbeat).unwrap();
    fleet.verify_and_promote_staged(Status::Canary, &observed);
    fleet
        .auth
        .manage(
            &fleet.admin,
            &Manage {
                node_key: fleet.active.node_key.clone(),
                operation_id: fleet.active.alias.clone(),
                action: Action::Drain,
            },
            &fleet.signer,
        )
        .unwrap();

    let updated = fleet.target_keys(0);
    assert!(updated.contains(&fleet.active.node_key) && updated.contains(&fleet.staged.node_key));
    assert!(!updated.contains(&fleet.disabled.node_key));
}

#[test]
fn record_cycle_ignores_staging_and_disabled_nodes_so_promotion_starts_clean() {
    let mut fleet = MonitoringFleet::new();
    let observed = fleet.head_digest();

    // Even if vantages submit failing observations for staging or disabled nodes for 3 cycles,
    // record_cycle ignores them so fresh promotions enter Listed without poisoned streaks.
    for _ in 0..3 {
        fleet.auth.environment.now += CYCLE_SECONDS;
        let at = fleet.auth.environment.now;
        let nodes = json!({
            fleet.active.node_key.clone(): {
                "operation_id": fleet.active.alias, "tcp": true, "https": true,
                "tls_expires": at + 86400, "relay": true, "stun": true
            },
            fleet.disabled.node_key.clone(): {
                "operation_id": fleet.disabled.alias, "tcp": false, "https": false,
                "tls_expires": null, "relay": false, "stun": false
            },
            fleet.staged.node_key.clone(): {
                "operation_id": fleet.staged_op, "tcp": false, "https": false,
                "tls_expires": null, "relay": false, "stun": false
            },
        });
        for index in 0..fleet.keys.len() {
            let mirror = (index < 2).then_some(observed.as_str());
            let report = proof(
                &fleet.keys[index],
                b"windshare/fleet-cycle/v1\0",
                &json!({"at": at, "mirror": mirror, "nodes": nodes}),
            );
            fleet.auth.record_cycle(&fleet.agents, &report).unwrap();
        }
    }
    for cycle in crate::fleet::monitoring::reports(&fleet.auth.store, &fleet.agents).unwrap() {
        assert!(cycle.nodes.contains_key(&fleet.active.node_key));
        assert!(
            !cycle.nodes.contains_key(&fleet.disabled.node_key)
                && !cycle.nodes.contains_key(&fleet.staged.node_key)
        );
    }

    fleet.verify_and_promote_staged(Status::Canary, &observed);
    let mut head = crate::fleet::publication::current(&fleet.auth.store).unwrap();
    fleet
        .auth
        .evaluate_health(&fleet.agents, &fleet.signer, &mut head)
        .unwrap();

    let published = head
        .payload()
        .unwrap()
        .nodes
        .into_iter()
        .find(|entry| entry.spec.node_key == fleet.staged.node_key)
        .unwrap();
    assert_eq!(published.status, Status::Canary);
    assert_eq!(published.spec.roles, [Role::Relay, Role::Stun]);
    let staged_key = crate::fleet::topology::public_key(&fleet.staged.node_key).unwrap();
    let saved: Node = load(&fleet.auth.store, &key("node", &staged_key))
        .unwrap()
        .unwrap();
    assert_eq!(saved.admission, Admission::Accepting);
    assert!(matches!(
        saved.listing.unwrap().state,
        ListingState::Listed {
            status: Status::Canary,
            ref withdrawn,
        } if withdrawn.is_empty()
    ));
}

#[test]
fn cycle_spacing_and_streaks_track_receipt_elapsed_time_across_epoch_boundaries() {
    let mut fleet = MonitoringFleet::new();
    let bad_relay = [false, true];
    let boundary = (fleet.auth.environment.now / CYCLE_SECONDS + 2) * CYCLE_SECONDS;

    // Start 1 s before a 300 s UTC boundary (bucket N), jump across two buckets on
    // the next report (bucket N+2 after +302 s), then land inside bucket N+2 again
    // after +298 s. Elapsed receipt time preserves streaks without false 400s.
    fleet.auth.environment.now = boundary - 1;
    fleet.observe_active_after(0, &[0, 1], bad_relay);
    assert_eq!(fleet.active_status(), Status::Active);

    fleet.observe_active_after(CYCLE_SECONDS + 2, &[0, 1], bad_relay);
    assert_eq!(fleet.active_status(), Status::Active);

    fleet.observe_active_after(CYCLE_SECONDS - 2, &[0, 1], bad_relay);
    assert_eq!(fleet.active_status(), Status::Draining);

    // Reports arriving sooner than MIN_CYCLE_SECONDS are rejected, and a previously
    // accepted fast-clock (+80 s) proof cannot be replayed once spacing elapses.
    let mut fleet = MonitoringFleet::new();
    let t0 = fleet.auth.environment.now;
    let fast_initial = fleet.active_report(0, t0 + 80, bad_relay);
    fleet
        .auth
        .record_cycle(&fleet.agents, &fast_initial)
        .unwrap();

    fleet.auth.environment.now = t0 + MIN_CYCLE_SECONDS - 1;
    let too_soon = fleet.active_report(0, fleet.auth.environment.now, bad_relay);
    assert!(fleet.auth.record_cycle(&fleet.agents, &too_soon).is_err());

    fleet.auth.environment.now = t0 + MIN_CYCLE_SECONDS;
    assert!(
        fleet
            .auth
            .record_cycle(&fleet.agents, &fast_initial)
            .is_err()
    );
    let spaced = fleet.active_report(0, fleet.auth.environment.now, bad_relay);
    fleet.auth.record_cycle(&fleet.agents, &spaced).unwrap();

    // Exceeding SILENT_SECONDS resets accumulated streaks instead of carrying stale evidence.
    let mut fleet = MonitoringFleet::new();
    fleet.observe_active_after(CYCLE_SECONDS, &[0, 1], bad_relay);
    fleet.observe_active_after(CYCLE_SECONDS, &[0, 1], bad_relay);
    fleet.observe_active_after(SILENT_SECONDS + 1, &[0, 1], bad_relay);
    assert_eq!(fleet.active_status(), Status::Active);
}
