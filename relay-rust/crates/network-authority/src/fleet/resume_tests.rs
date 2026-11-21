use super::*;
use crate::fleet::{
    Promote, attach,
    maintenance::Operation,
    operations::{Action, Admission, Manage},
    publication,
    topology::{Entry, Role},
};
use std::collections::BTreeSet;

struct ResumeFleet {
    auth: Authority<Memory, Clock>,
    admin: String,
    signer: Publisher,
    cn_key: SigningKey,
    agents: Vec<Agent>,
    target: Spec,
}

impl ResumeFleet {
    fn new(initial: Status, roles: &[Role]) -> Self {
        let mut auth = fixture();
        let admin = auth
            .initialize("init", "init", "operator", "reasonable password")
            .unwrap();
        let (release, signer, history) = directory();
        Publication::bootstrap(&mut auth.store, &release, &[&history]).unwrap();
        let node_key = SigningKey::from_bytes(&[31; 32]);
        let mut value = spec("target", 31);
        value["node_key"] = json!(URL_SAFE_NO_PAD.encode(node_key.verifying_key().to_bytes()));
        value["roles"] = json!(roles);
        value["stun"] = if roles.contains(&Role::Stun) {
            json!(["stun:target.example:3478"])
        } else {
            json!([])
        };
        let target: Spec = serde_json::from_value(value).unwrap();
        attach(
            &mut auth.store,
            target.clone(),
            ListingState::Listed {
                status: initial,
                withdrawn: BTreeSet::new(),
            },
            "target-op".into(),
        )
        .unwrap();
        let mut head = publication::current(&auth.store).unwrap();
        let now = auth.environment.now();
        head.project(&mut auth.store, &signer, now, "init").unwrap();
        let cn_key = SigningKey::from_bytes(&[32; 32]);
        let agents = vec![cn_agent(&cn_key)];
        Self {
            auth,
            admin,
            signer,
            cn_key,
            agents,
            target,
        }
    }

    fn node(&self, node_key: &str) -> Node {
        let id = crate::fleet::topology::public_key(node_key).unwrap();
        load::<Node>(&self.auth.store, &key("node", &id))
            .unwrap()
            .unwrap()
    }

    fn entry(&self, alias: &str) -> Entry {
        publication::current(&self.auth.store)
            .unwrap()
            .payload()
            .unwrap()
            .nodes
            .into_iter()
            .find(|e| e.spec.alias == alias)
            .unwrap()
    }

    fn serial(&self) -> u64 {
        publication::current(&self.auth.store)
            .unwrap()
            .revision()
            .unwrap()
            .serial
    }

    fn manage(&mut self, node_key: &str, action: Action) -> anyhow::Result<()> {
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

    fn promote(&mut self, node_key: &str, status: Status) -> anyhow::Result<()> {
        let operation_id = self.node(node_key).listing.unwrap().operation_id;
        let observed = digest(&publication::current(&self.auth.store).unwrap().envelope).unwrap();
        self.auth.promote(
            &self.admin,
            &Promote {
                node_key: node_key.into(),
                operation_id,
                status,
            },
            &self.agents,
            &observed,
            &self.signer,
        )
    }

    fn report_current_mirror(&mut self) {
        self.auth.environment.now += crate::fleet::monitoring::CYCLE_SECONDS;
        let at = self.auth.environment.now;
        let mirror = digest(&publication::current(&self.auth.store).unwrap().envelope).unwrap();
        let report = proof(
            &self.cn_key,
            b"windshare/fleet-cycle/v1\0",
            &json!({"at": at, "mirror": mirror, "nodes": {}}),
        );
        self.auth.record_cycle(&self.agents, &report).unwrap();
    }
}

#[test]
fn resume_restores_draining_node_immediately_without_mirror_propagation() {
    let mut fleet = ResumeFleet::new(Status::Active, &[Role::Relay, Role::Stun]);
    let target_key = fleet.target.node_key.clone();

    // No CN vantage has reported yet, so mainland mirrors are lagging/unknown.
    fleet.manage(&target_key, Action::Drain).unwrap();
    assert_eq!(fleet.node(&target_key).admission, Admission::Draining);
    assert_eq!(fleet.entry("target").status, Status::Draining);

    // promote() rejects Draining listings with Invalid rather than verification_required or mirror_lag.
    let err = fleet.promote(&target_key, Status::Active).unwrap_err();
    assert!(matches!(err.downcast_ref::<Fault>(), Some(Fault::Invalid)));

    // Action::Resume restores the node immediately without waiting for any vantage cycle.
    let serial_before_resume = fleet.serial();
    let events = super::observability::capture_events(|| {
        fleet.manage(&target_key, Action::Resume).unwrap();
    });
    assert_eq!(fleet.node(&target_key).admission, Admission::Accepting);
    assert_eq!(fleet.entry("target").status, Status::Active);
    assert_eq!(fleet.serial(), serial_before_resume + 1);
    let intent_events = super::observability::events_named(&events, "node_intent_changed");
    assert_eq!(intent_events.len(), 1);
    assert_eq!(intent_events[0]["fields"]["node_id"], target_key);
    assert_eq!(intent_events[0]["fields"]["action"], "Resume");

    // Repeating Action::Resume on an Active node is an idempotent no-op.
    let version = fleet.node(&target_key).version;
    let serial = fleet.serial();
    fleet.manage(&target_key, Action::Resume).unwrap();
    assert_eq!(fleet.node(&target_key).version, version);
    assert_eq!(fleet.serial(), serial);
}

#[test]
fn resume_rejects_canary_staging_and_disabled_nodes() {
    let mut fleet = ResumeFleet::new(Status::Canary, &[Role::Relay]);
    let canary_key = fleet.target.node_key.clone();

    // Canary cannot bypass promote()'s mirror-freshness gate via Action::Resume.
    let err = fleet.manage(&canary_key, Action::Resume).unwrap_err();
    assert!(matches!(err.downcast_ref::<Fault>(), Some(Fault::Invalid)));
    assert_eq!(fleet.entry("target").status, Status::Canary);

    // promote(Canary -> Active) still enforces mirror_lag, then succeeds once mirrors match.
    let err = fleet.promote(&canary_key, Status::Active).unwrap_err();
    assert!(matches!(
        err.downcast_ref::<Fault>(),
        Some(Fault::Denied { reason }) if reason == "mirror_lag"
    ));
    fleet.report_current_mirror();
    fleet.promote(&canary_key, Status::Active).unwrap();
    assert_eq!(fleet.entry("target").status, Status::Active);

    // Disabled nodes reject Action::Resume and require re-verification before promote().
    fleet
        .manage(&canary_key, Action::Disable { early_cutoff: true })
        .unwrap();
    let err = fleet.manage(&canary_key, Action::Resume).unwrap_err();
    assert!(matches!(err.downcast_ref::<Fault>(), Some(Fault::Invalid)));
    let err = fleet.promote(&canary_key, Status::Active).unwrap_err();
    assert!(matches!(
        err.downcast_ref::<Fault>(),
        Some(Fault::Denied { reason }) if reason == "verification_required"
    ));

    // Staging drafts (both Queued and Passed) reject Action::Resume.
    let draft_key = SigningKey::from_bytes(&[33; 32]);
    let mut draft_val = spec("draft", 33);
    draft_val["node_key"] = json!(URL_SAFE_NO_PAD.encode(draft_key.verifying_key().to_bytes()));
    let draft: Spec = serde_json::from_value(draft_val).unwrap();
    fleet.auth.stage(&fleet.admin, draft.clone()).unwrap();
    let err = fleet.manage(&draft.node_key, Action::Resume).unwrap_err();
    assert!(matches!(err.downcast_ref::<Fault>(), Some(Fault::Invalid)));

    fleet.auth.environment.now += 1;
    let at = fleet.auth.environment.now;
    let shares = [[1u8; 12], [2u8; 12], [3u8; 12]];
    let poll = proof(
        &fleet.cn_key,
        b"windshare/vantage-poll/v1\0",
        &json!({"at": at, "shares": shares}),
    );
    let work = fleet
        .auth
        .claim(&fleet.agents, &poll, Default::default())
        .unwrap()
        .unwrap();
    let passed = proof(
        &fleet.cn_key,
        b"windshare/vantage-result/v1\0",
        &json!({"at": at, "node_key": draft.node_key, "lease": work.lease, "outcome": {"kind": "passed"}}),
    );
    fleet.auth.complete_probe(&fleet.agents, &passed).unwrap();
    let err = fleet.manage(&draft.node_key, Action::Resume).unwrap_err();
    assert!(matches!(err.downcast_ref::<Fault>(), Some(Fault::Invalid)));
}

#[test]
fn resume_preserves_maintenance_hold_and_stun_only_admission() {
    let mut fleet = ResumeFleet::new(Status::Draining, &[Role::Relay, Role::Stun]);
    let target_key = fleet.target.node_key.clone();
    let op = Operation {
        node_key: target_key.clone(),
        operation_id: "resume-during-hold-01".into(),
    };

    fleet
        .auth
        .maintenance_hold(&op, true, &fleet.signer)
        .unwrap();
    fleet.manage(&target_key, Action::Resume).unwrap();
    // Intent becomes Active, but the active maintenance hold keeps traffic draining until release.
    assert_eq!(fleet.node(&target_key).admission, Admission::Draining);
    assert_eq!(fleet.entry("target").status, Status::Draining);

    fleet
        .auth
        .maintenance_hold(&op, false, &fleet.signer)
        .unwrap();
    assert_eq!(fleet.node(&target_key).admission, Admission::Accepting);
    assert_eq!(fleet.entry("target").status, Status::Active);

    // STUN-only nodes drained by an aborted upgrade resume to Active while staying relay-draining.
    let mut stun_fleet = ResumeFleet::new(Status::Active, &[Role::Stun]);
    let stun_key = stun_fleet.target.node_key.clone();
    let stun_op = Operation {
        node_key: stun_key.clone(),
        operation_id: "stun-abort-resume-001".into(),
    };
    stun_fleet
        .auth
        .maintenance_hold(&stun_op, true, &stun_fleet.signer)
        .unwrap();
    stun_fleet
        .manage(
            &stun_key,
            Action::AbortMaintenance {
                maintenance_id: stun_op.operation_id,
            },
        )
        .unwrap();
    assert_eq!(stun_fleet.entry("target").status, Status::Draining);

    stun_fleet.manage(&stun_key, Action::Resume).unwrap();
    assert_eq!(stun_fleet.entry("target").status, Status::Active);
    assert_eq!(stun_fleet.entry("target").spec.roles, [Role::Stun]);
    assert_eq!(stun_fleet.node(&stun_key).admission, Admission::Draining);
}
