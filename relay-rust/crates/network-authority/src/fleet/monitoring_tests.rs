use super::*;
use crate::fleet::{
    Promote, attach,
    monitoring::{CYCLE_SECONDS, MIN_CYCLE_SECONDS},
    operations::{Action, Admission, Manage},
    topology::Role,
};
use tracing::Level;

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
                NodeState::Listed {
                    status: Status::Active,
                    maintenance: None,
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
        let staged_op = auth.stage(&admin, staged.clone()).unwrap().operation_id;
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
        let req = proof(
            &self.keys[vantage],
            b"windshare/fleet-targets/v1\0",
            &json!({"at": self.auth.environment.now()}),
        );
        self.auth
            .fleet_targets(&self.agents, &req)
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

    fn active_report(&self, vantage: usize, reported_at: u64, active_roles: [bool; 2]) -> Proof {
        let prior_key = URL_SAFE_NO_PAD.encode([9u8; 32]);
        let prior_op = load::<Node>(&self.auth.store, &key("node", &[9u8; 32]))
            .unwrap()
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
fn record_cycle_records_read_only_observations_and_ignores_unlisted_nodes() {
    let mut fleet = MonitoringFleet::new();
    let observed = fleet.head_digest();

    for _ in 0..3 {
        fleet.auth.environment.now += CYCLE_SECONDS;
        let at = fleet.auth.environment.now;
        let nodes = json!({
            fleet.active.node_key.clone(): {
                "operation_id": fleet.active.alias, "tcp": true, "https": true,
                "tls_expires": at + 86400, "relay": false, "stun": true
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
    let reports = crate::fleet::monitoring::reports(&fleet.auth.store, &fleet.agents).unwrap();
    assert_eq!(reports.len(), fleet.agents.len());
    for cycle in reports {
        let active_obs = cycle.nodes.get(&fleet.active.node_key).unwrap();
        assert_eq!(active_obs.relay, Some(false));
        assert_eq!(active_obs.stun, Some(true));
        assert!(
            !cycle.nodes.contains_key(&fleet.disabled.node_key)
                && !cycle.nodes.contains_key(&fleet.staged.node_key)
        );
    }

    // Monitoring observations remain read-only: failing probes do not mutate node admission
    // or directory status, leaving traffic changes to explicit operator actions.
    let active_key = crate::fleet::topology::public_key(&fleet.active.node_key).unwrap();
    let active_saved: Node = load(&fleet.auth.store, &key("node", &active_key))
        .unwrap()
        .unwrap();
    assert_eq!(active_saved.admission(), Admission::Accepting);
    assert!(matches!(
        active_saved.state(),
        NodeState::Listed {
            status: Status::Active,
            maintenance: None,
        }
    ));

    fleet.verify_and_promote_staged(Status::Canary, &observed);
    let head = crate::fleet::publication::current(&fleet.auth.store).unwrap();
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
    assert_eq!(saved.admission(), Admission::Accepting);
    assert!(matches!(
        saved.state(),
        NodeState::Listed {
            status: Status::Canary,
            maintenance: None,
        }
    ));
}

#[test]
fn cycle_spacing_rejects_replays_and_warns_only_on_unexpected_probe_failures() {
    let mut fleet = MonitoringFleet::new();
    let bad_relay = [false, true];
    let t0 = fleet.auth.environment.now;

    // An accepting node reporting relay=false emits a structured fleet_probe_failed warning.
    let fast_initial = fleet.active_report(0, t0 + 80, bad_relay);
    let events = super::observability::capture_events(|| {
        fleet
            .auth
            .record_cycle(&fleet.agents, &fast_initial)
            .unwrap();
    });
    let warnings: Vec<_> = events
        .iter()
        .filter(|(level, entry)| {
            *level == Level::WARN && entry["fields"]["event"] == "fleet_probe_failed"
        })
        .collect();
    assert_eq!(warnings.len(), 1);
    assert_eq!(warnings[0].1["fields"]["node_id"], fleet.active.node_key);

    // Reports arriving sooner than MIN_CYCLE_SECONDS are rejected, and a previously
    // accepted fast-clock (+80 s) proof cannot be replayed once spacing elapses.
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

    // Once drained by the operator, relay=false reflects the intended admission state and
    // does not emit a false-positive fleet_probe_failed warning, whereas a STUN failure does.
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
    let drained_relay = fleet.active_report(0, fleet.auth.environment.now, bad_relay);
    let events = super::observability::capture_events(|| {
        fleet
            .auth
            .record_cycle(&fleet.agents, &drained_relay)
            .unwrap();
    });
    assert_eq!(
        super::observability::events_named(&events, "fleet_probe_failed"),
        Vec::<&serde_json::Value>::new()
    );

    fleet.auth.environment.now += CYCLE_SECONDS;
    let drained_stun_fault = fleet.active_report(0, fleet.auth.environment.now, [false, false]);
    let events = super::observability::capture_events(|| {
        fleet
            .auth
            .record_cycle(&fleet.agents, &drained_stun_fault)
            .unwrap();
    });
    assert_eq!(
        super::observability::events_named(&events, "fleet_probe_failed").len(),
        1
    );
}
