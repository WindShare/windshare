use super::*;
use crate::{
    config::limits,
    environment::Trace,
    storage::{Database, Persistence},
    test_support::{Fixture, work},
};
use windshare_relay_access::{probe, trust::Generation, usage::NodePolicy};
use windshare_relay_core::share::{Responsibility, Restore, ShareRelay};

#[test]
fn durable_fence_requires_the_installed_key_current_drain_and_no_pending_join() {
    let temp = tempfile::tempdir().unwrap();
    let issuer = [3u8; 32];
    let config: Configuration = serde_json::from_value(serde_json::json!({
        "listen":"127.0.0.1:8484", "database":temp.path().join("relay.db"),
        "identity_file":temp.path().join("identity.json"),
        "node":{"services":{"relay":true,"stun":false},"authority":{"url":"https://authority.example","issuer":URL_SAFE_NO_PAD.encode(issuer)}}
    })).unwrap();
    let identity = Identity::open(&config.identity_file).unwrap();
    let node = URL_SAFE_NO_PAD.encode(identity.key().verifying_key().to_bytes());
    check_identity(&config, &node).unwrap();
    assert!(check_identity(&config, &URL_SAFE_NO_PAD.encode([7; 32])).is_err());
    assert!(!config.database.exists());

    let fixture = Fixture::new();
    let db = Database::open(&config.database, fixture.identity().relay).unwrap();
    db.initialize_control(100).unwrap();
    db.save_status(
        AuthorityState {
            generation: Generation {
                issuer,
                number: 2,
                key: [11; 32],
                not_before: 1,
                expires_at: 200,
            },
            policy: Some(NodePolicy {
                version: 7,
                admission: Admission::Draining,
                anonymous: true,
                probe_account: probe::PROBE_ACCOUNT,
            }),
        },
        vec![],
        100,
    )
    .unwrap();
    let owner = "operation-owner-1";
    let other = "operation-other-2";
    let enter = |policy_version| Transition::Enter {
        policy_version,
        reconnect_budget: Duration::from_secs(56),
    };
    let gate = Gate::install(&config.database).unwrap();
    assert!(change(&config, &node, owner, enter(6)).is_err());
    let join = gate.admit(false).unwrap();
    assert!(!change(&config, &node, owner, enter(7)).unwrap());
    drop(join);
    assert!(change(&config, &node, owner, enter(7)).unwrap());
    check_hold(&config, &node, owner).unwrap();
    assert!(check_hold(&config, &node, other).is_err());
    drop(db);
    let _db = Database::open(&config.database, fixture.identity().relay).unwrap();
    let restarted = Gate::install(&config.database).unwrap();
    assert!(restarted.admit(false).is_err());
    assert!(restarted.admit(true).is_ok());
    assert!(change(&config, &node, other, Transition::Release).is_err());
    assert!(change(&config, &node, owner, Transition::Release).unwrap());
    assert!(restarted.admit(false).is_ok());
    assert!(change(&config, &node, owner, Transition::Release).unwrap());
}

#[test]
fn already_disconnected_sender_must_have_time_for_rollback_and_redial() {
    let temp = tempfile::tempdir().unwrap();
    let path = temp.path().join("relay.db");
    let fixture = Fixture::new();
    let id = fixture.identity();
    let db = Database::open(&path, id.relay).unwrap();
    let published = db.execute(id.share, &fixture.works()[0]).unwrap().unwrap();
    let now = fixture.now(false);
    let (_, step) = ShareRelay::restore(
        id,
        limits(),
        &Restore::restarted(
            Some(published.record()),
            Responsibility::Released { previous: None },
        ),
        Trace,
        now,
    )
    .unwrap();
    let stored = db
        .execute(id.share, &work(step).operation)
        .unwrap()
        .unwrap();
    let Record::Grace(grace) = stored.record() else {
        panic!("sender did not enter grace");
    };
    let reader = Connection::open_with_flags(path, OpenFlags::SQLITE_OPEN_READ_ONLY).unwrap();
    assert!(has_reconnect_window(&reader, grace.deadline()).unwrap());
    assert!(
        !has_reconnect_window(
            &reader,
            grace.deadline().saturating_add(Duration::from_millis(1))
        )
        .unwrap()
    );
}
