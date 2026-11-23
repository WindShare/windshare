use super::*;
use crate::fleet::{
    operations::{Action, Manage},
    publication,
    topology::Role,
};
use windshare_relay_access::usage::{Admission, NodePolicy};

fn command(node: &Node, action: Action) -> Manage {
    Manage {
        node_key: node.spec.node_key.clone(),
        expected_state: node.state(),
        expected_policy_version: node.version,
        action,
    }
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
    assert!(auth.manage(&admin, &activate, &signer).is_err());
    let stale = command(&active, Action::Drain);
    let current = auth
        .set_node_policy(&admin, &node.key, NodePolicyChange::AnonymousSharing(true))
        .unwrap();
    assert!(auth.manage(&admin, &stale, &signer).is_err());
    auth.manage(&admin, &command(&current, Action::Drain), &signer)
        .unwrap();
    let draining = auth.node_by_key(&admin, &node.key).unwrap().node;
    assert_eq!(draining.admission(), Admission::Draining);
    assert!(
        auth.manage(
            &admin,
            &command(
                &draining,
                Action::Disable {
                    early_cutoff: false
                }
            ),
            &signer
        )
        .is_err()
    );
    auth.manage(
        &admin,
        &command(&draining, Action::Disable { early_cutoff: true }),
        &signer,
    )
    .unwrap();
    let disabled = auth.node_by_key(&admin, &node.key).unwrap().node;
    assert!(
        auth.manage(
            &admin,
            &command(
                &disabled,
                Action::Remove {
                    early_cutoff: false
                }
            ),
            &signer
        )
        .is_err()
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
    assert!(auth.enroll(&admin, reassigned, &signer).is_err());
    assert_eq!(
        auth.enroll(&admin, spec, &signer).unwrap().version,
        disabled.version + 1
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
    assert!(
        auth.manage(&admin, &command(&node, Action::Drain), &signer)
            .is_err()
    );
    auth.manage(&admin, &command(&node, Action::Activate), &signer)
        .unwrap();
    let active = auth.node_by_key(&admin, &node.key).unwrap().node;
    assert_eq!(active.admission(), Admission::Draining);
    assert_eq!(active.version, node.version);
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
    ] {
        let mut legacy = serde_json::to_value(listed_node([id; 32], Zone::Global, "test")).unwrap();
        legacy["state"] = state;
        legacy["operation_id"] = json!("legacy");
        legacy["version"] = json!(7);
        save(&mut store, &key("node", &[id; 32]), &legacy).unwrap();
    }
    crate::fleet::migration::migrate(&mut store).unwrap();
    let active: Node = load(&store, &key("node", &[9; 32])).unwrap().unwrap();
    let disabled: Node = load(&store, &key("node", &[10; 32])).unwrap().unwrap();
    let retired: NodePolicy = load(&store, &key("retired-node", &[11; 32]))
        .unwrap()
        .unwrap();
    assert_eq!((active.state(), active.version), (NodeState::Active, 7));
    assert_eq!(
        (disabled.state(), disabled.version),
        (NodeState::Disabled, 8)
    );
    assert_eq!(
        (retired.admission, retired.version),
        (Admission::Disabled, 8)
    );
    assert!(store.get(&key("node", &[11; 32])).unwrap().is_none());
    assert_eq!(
        publication::current(&store).unwrap().envelope,
        head.envelope
    );
    let writes = store.put_count;
    crate::fleet::migration::migrate(&mut store).unwrap();
    assert_eq!(store.put_count, writes);
}
