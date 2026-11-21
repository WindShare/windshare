use super::*;
use crate::{
    config::limits,
    environment::Trace,
    storage::{Database, Persistence},
    test_support::{Fixture, capture_events, work},
};
use windshare_relay_access::{probe, trust::Generation, usage::NodePolicy};
use windshare_relay_core::share::{Responsibility, Restore, ShareRelay};

fn draining_node(temp: &tempfile::TempDir) -> (Configuration, String, Database) {
    let issuer = [3u8; 32];
    let config: Configuration = serde_json::from_value(serde_json::json!({
        "listen": "127.0.0.1:8484",
        "database": temp.path().join("relay.db"),
        "identity_file": temp.path().join("identity.json"),
        "node": {
            "services": { "relay": true, "stun": false },
            "authority": {
                "url": "https://authority.example",
                "issuer": URL_SAFE_NO_PAD.encode(issuer),
            }
        }
    }))
    .unwrap();
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
    (config, node, db)
}

#[test]
fn durable_fence_requires_the_installed_key_current_drain_and_no_pending_join() {
    let temp = tempfile::tempdir().unwrap();
    let (config, node, db) = draining_node(&temp);

    let fixture = Fixture::new();
    let owner = "operation-owner-1";
    let other = "operation-other-2";
    let enter = |policy_version| Transition::Enter {
        policy_version,
        reconnect_budget: Duration::from_secs(56),
    };
    let gate = Gate::install(&config.database).unwrap();
    assert!(gate.open_to_users());
    assert!(change(&config, &node, owner, enter(6)).is_err());
    let join = gate.admit_join(false).unwrap();
    assert!(change(&config, &node, owner, enter(6)).is_err());
    assert!(!change(&config, &node, owner, enter(7)).unwrap());
    drop(join);
    assert!(change(&config, &node, owner, enter(7)).unwrap());
    assert!(!gate.open_to_users());
    check_hold(&config, &node, owner).unwrap();
    assert!(check_hold(&config, &node, other).is_err());
    drop(db);
    let _db = Database::open(&config.database, fixture.identity().relay).unwrap();
    let restarted = Gate::install(&config.database).unwrap();
    assert!(matches!(
        restarted.admit_join(false),
        Err(GateError::Maintenance)
    ));
    assert!(matches!(
        restarted.check(false),
        Err(GateError::Maintenance)
    ));
    assert!(restarted.admit_join(true).is_ok());
    assert!(restarted.check(true).is_ok());
    assert!(change(&config, &node, other, Transition::Release).is_err());
    assert!(change(&config, &node, owner, Transition::Release).unwrap());
    assert!(restarted.open_to_users());
    assert!(restarted.admit_join(false).is_ok());
    assert!(change(&config, &node, owner, Transition::Release).unwrap());
}

#[test]
fn shared_gate_refcounts_leases_preserves_probe_only_and_reports_io_errors() {
    let temp = tempfile::tempdir().unwrap();
    let (config, node, _db) = draining_node(&temp);
    let owner = "operation-owner-1";
    let enter = Transition::Enter {
        policy_version: 7,
        reconnect_budget: Duration::from_secs(56),
    };
    let gate = Gate::install(&config.database).unwrap();

    // Multiple concurrent receivers share one process-wide lock until the last drops.
    let first = gate.admit_join(false).unwrap();
    let second = gate.admit_join(false).unwrap();
    assert!(gate.check(false).is_ok());
    assert!(gate.open_to_users());
    drop(first);
    assert!(!change(&config, &node, owner, enter).unwrap());
    drop(second);
    assert!(change(&config, &node, owner, enter).unwrap());

    // An active probe lease during maintenance must still refuse ordinary users.
    let probe_first = gate.admit_join(true).unwrap();
    assert!(matches!(
        gate.admit_join(false),
        Err(GateError::Maintenance)
    ));
    assert!(matches!(gate.check(false), Err(GateError::Maintenance)));
    assert!(!gate.open_to_users());
    let probe_second = gate.admit_join(true).unwrap();
    assert!(gate.check(true).is_ok());
    drop(probe_first);
    assert!(!change(&config, &node, owner, Transition::Release).unwrap());
    drop(probe_second);
    assert!(change(&config, &node, owner, Transition::Release).unwrap());
    assert!(gate.open_to_users());

    // Unexpected OS I/O errors remain distinct from maintenance refusals and log warnings.
    gate.set_io_fault(Some(std::io::ErrorKind::PermissionDenied));
    assert!(matches!(
        gate.admit_join(false),
        Err(GateError::Io(ref err)) if err.kind() == std::io::ErrorKind::PermissionDenied
    ));
    assert!(matches!(
        gate.admit_join(true),
        Err(GateError::Io(ref err)) if err.kind() == std::io::ErrorKind::PermissionDenied
    ));
    assert!(matches!(
        gate.check(false),
        Err(GateError::Io(ref err)) if err.kind() == std::io::ErrorKind::PermissionDenied
    ));
    let events = capture_events(|| {
        assert!(!gate.open_to_users());
    });
    assert!(events.iter().any(|entry| {
        entry["level"] == "WARN"
            && entry["fields"]["event"] == "maintenance_gate_io_error"
            && entry["fields"]["operation"] == "open_to_users"
    }));
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
    let reader = crate::storage::open_read_only(&path, READ_BUSY_TIMEOUT).unwrap();
    assert!(has_reconnect_window(&reader, grace.deadline()).unwrap());
    assert!(
        !has_reconnect_window(
            &reader,
            grace.deadline().saturating_add(Duration::from_millis(1))
        )
        .unwrap()
    );
}

#[test]
fn busy_sqlite_defers_enter_and_waits_for_transient_lock_release() {
    let temp = tempfile::tempdir().unwrap();
    let issuer = [5u8; 32];
    let config: Configuration = serde_json::from_value(serde_json::json!({
        "listen": "127.0.0.1:8484",
        "database": temp.path().join("relay.db"),
        "identity_file": temp.path().join("identity.json"),
        "node": {
            "services": {"relay": true, "stun": false},
            "authority": {
                "url": "https://authority.example",
                "issuer": URL_SAFE_NO_PAD.encode(issuer)
            }
        }
    }))
    .unwrap();
    let identity = Identity::open(&config.identity_file).unwrap();
    let node = URL_SAFE_NO_PAD.encode(identity.key().verifying_key().to_bytes());
    let fixture = Fixture::new();
    let db = Database::open(&config.database, fixture.identity().relay).unwrap();
    db.initialize_control(100).unwrap();
    db.save_status(
        AuthorityState {
            generation: Generation {
                issuer,
                number: 1,
                key: [11; 32],
                not_before: 1,
                expires_at: 200,
            },
            policy: Some(NodePolicy {
                version: 9,
                admission: Admission::Draining,
                anonymous: true,
                probe_account: probe::PROBE_ACCOUNT,
            }),
        },
        vec![],
        100,
    )
    .unwrap();
    let gate = Gate::install(&config.database).unwrap();
    drop(db);

    // Hold an exclusive SQLite lock like WAL recovery or startup journal setup.
    let blocker = Connection::open(&config.database).unwrap();
    blocker
        .execute_batch(
            "PRAGMA locking_mode = EXCLUSIVE;
             BEGIN IMMEDIATE;
             UPDATE access_authorities SET body = body;
             COMMIT;",
        )
        .unwrap();

    let owner = "operation-owner-1";
    let enter = Transition::Enter {
        policy_version: 9,
        reconnect_budget: Duration::from_secs(56),
    };
    let started = std::sync::Barrier::new(2);
    let entered = std::thread::scope(|scope| {
        let deferred = scope.spawn(|| {
            started.wait();
            change_with_read_timeout(&config, &node, owner, enter, Duration::from_millis(120))
        });
        started.wait();
        std::thread::sleep(Duration::from_millis(30));
        // Waiting on SQLite must not hold the exclusive `.admission` lock or refuse
        // concurrent RESUME/JOIN handshakes before maintenance is actually entered.
        assert!(gate.check(false).is_ok());
        let concurrent_join = gate.admit_join(false).unwrap();
        drop(concurrent_join);
        deferred.join().unwrap()
    })
    .unwrap();
    assert!(!entered);
    assert!(check_hold(&config, &node, owner).is_err());
    assert!(gate.open_to_users());

    // Releasing the exclusive lock within busy_timeout lets enter succeed
    // cleanly without waiting for an external poll cycle.
    let release = std::thread::spawn(move || {
        std::thread::sleep(Duration::from_millis(30));
        drop(blocker);
    });
    assert!(
        change_with_read_timeout(&config, &node, owner, enter, Duration::from_secs(2)).unwrap()
    );
    release.join().unwrap();
    check_hold(&config, &node, owner).unwrap();
}

#[test]
fn non_busy_sqlite_errors_remain_fatal() {
    let temp = tempfile::tempdir().unwrap();
    let issuer = [6u8; 32];
    let config: Configuration = serde_json::from_value(serde_json::json!({
        "listen": "127.0.0.1:8484",
        "database": temp.path().join("relay.db"),
        "identity_file": temp.path().join("identity.json"),
        "node": {
            "services": {"relay": true, "stun": false},
            "authority": {
                "url": "https://authority.example",
                "issuer": URL_SAFE_NO_PAD.encode(issuer)
            }
        }
    }))
    .unwrap();
    let identity = Identity::open(&config.identity_file).unwrap();
    let node = URL_SAFE_NO_PAD.encode(identity.key().verifying_key().to_bytes());
    let fixture = Fixture::new();
    let db = Database::open(&config.database, fixture.identity().relay).unwrap();
    db.initialize_control(100).unwrap();
    let _gate = Gate::install(&config.database).unwrap();
    drop(db);

    // Missing authority row is QueryReturnedNoRows, not transient lock contention.
    let err = change(
        &config,
        &node,
        "operation-owner-1",
        Transition::Enter {
            policy_version: 1,
            reconnect_budget: Duration::from_secs(56),
        },
    )
    .unwrap_err();
    assert!(!is_database_busy(&err));
    assert!(crate::storage::is_busy(&rusqlite::Error::SqliteFailure(
        rusqlite::ffi::Error::new(rusqlite::ffi::SQLITE_BUSY_RECOVERY),
        None,
    )));
    assert!(!crate::storage::is_busy(&rusqlite::Error::SqliteFailure(
        rusqlite::ffi::Error::new(rusqlite::ffi::SQLITE_LOCKED),
        None,
    )));
}
