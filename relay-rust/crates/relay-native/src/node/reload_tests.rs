use super::*;
use crate::{
    Database, Endpoint,
    node::{Identity, MonthlyBudget},
    test_support::capture_events,
};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use chrono::{DateTime, Months};
use ed25519_dalek::SigningKey;
use rusqlite::Connection;
use tempfile::TempDir;
use windshare_relay_access::trust::Generation;

const OLD_CAPACITY: u64 = 10_000_000;
const NEW_CAPACITY: u64 = 2_000_000;
const QUOTA: usize = 128;

fn configured_node() -> (TempDir, Endpoint, Runtime, Settings) {
    let directory = tempfile::tempdir().unwrap();
    let endpoint = Endpoint::parse("wss://node.example/v2/ws").unwrap();
    let store = Database::open(&directory.path().join("node.sqlite"), endpoint.identity).unwrap();
    let settings = Settings {
        ceiling: crate::node::Ceiling {
            bytes_per_second: Some(OLD_CAPACITY),
            ..Default::default()
        },
        relay_budget: Some(MonthlyBudget {
            bytes: QUOTA as u64,
            reset_day_utc: 1,
        }),
        ..Settings::default()
    };
    let node = Runtime::default();
    node.identify(Identity::generate().unwrap()).unwrap();
    node.install_settings(settings.clone(), store).unwrap();
    (directory, endpoint, node, settings)
}

fn describe(node: &Runtime, endpoint: &Endpoint) -> serde_json::Value {
    let body = node.descriptor(endpoint).unwrap();
    let payload = URL_SAFE_NO_PAD
        .decode(body["descriptor"].as_str().unwrap())
        .unwrap();
    serde_json::from_slice(&payload).unwrap()
}

fn assert_public_settings(node: &Runtime, endpoint: &Endpoint, capacity: u64, anonymous: bool) {
    assert_eq!(node.governor.capacity(), Some(capacity));
    assert_eq!(node.access.anonymous(), anonymous);
    let descriptor = describe(node, endpoint);
    assert_eq!(descriptor["public_capacity_bytes_per_second"], capacity);
    if node.available() {
        assert_eq!(descriptor["public_relay"]["anonymous"], anonymous);
    } else {
        assert_eq!(descriptor["public_relay"]["state"], "budget_exhausted");
    }
}

fn next_period(node: &Runtime) -> i64 {
    let store = node.state.lock().unwrap().control.clone().unwrap();
    let period = store.load_budgets().unwrap().monthly.unwrap().period;
    DateTime::from_timestamp(period, 0)
        .unwrap()
        .checked_add_months(Months::new(1))
        .unwrap()
        .timestamp()
}

fn change_public_settings(settings: &mut Settings) {
    settings.ceiling.bytes_per_second = Some(NEW_CAPACITY);
    settings.access.anonymous = false;
}

fn write_bytes(node: &Runtime, bytes: usize) -> std::io::Result<usize> {
    use crate::node::{WriteOrigin, budget::WriteAttempt};
    use std::task::Poll;
    let origin = WriteOrigin::test_public();
    node.refill_budgets(&origin, bytes as u64, now()).unwrap();
    let WriteAttempt::Socket(Poll::Ready(written)) =
        node.write_egress(node.epoch(), &origin, bytes, now(), |length| {
            Poll::Ready(Ok(length))
        })?
    else {
        panic!("funded writer did not complete");
    };
    written
}

fn exhaust_budget(node: &Runtime) {
    assert_eq!(write_bytes(node, QUOTA).unwrap(), QUOTA);
    assert!(write_bytes(node, 1).is_err());
    assert!(!node.available());
}

#[test]
fn rejected_reset_day_keeps_live_settings_and_descriptor() {
    let (_directory, endpoint, node, mut settings) = configured_node();
    let changes = node.access.changes();
    assert_eq!(write_bytes(&node, QUOTA - 1).unwrap(), QUOTA - 1);
    change_public_settings(&mut settings);
    settings.relay_budget.as_mut().unwrap().reset_day_utc = 15;

    let error = node.update(settings, now()).unwrap_err();
    assert!(
        error
            .to_string()
            .contains("billing reset day cannot change")
    );
    assert_public_settings(&node, &endpoint, OLD_CAPACITY, true);
    assert!(!changes.has_changed().unwrap());
    assert_eq!(write_bytes(&node, 1).unwrap(), 1);
}

#[test]
fn issuer_write_failure_rolls_back_the_pending_billing_period() {
    let (directory, endpoint, node, mut settings) = configured_node();
    exhaust_budget(&node);
    let changes = node.access.changes();
    let store = node.state.lock().unwrap().control.clone().unwrap();
    let previous_budget = store.load_budgets().unwrap().monthly.unwrap();

    let db = Connection::open(directory.path().join("node.sqlite")).unwrap();
    db.execute_batch(
        "CREATE TRIGGER reject_issuer BEFORE INSERT ON access_authorities
           BEGIN SELECT RAISE(ABORT, 'injected issuer write failure'); END;",
    )
    .unwrap();
    change_public_settings(&mut settings);
    let root = SigningKey::from_bytes(&[7; 32]);
    let generation = Generation {
        issuer: root.verifying_key().to_bytes(),
        number: 1,
        key: SigningKey::from_bytes(&[8; 32]).verifying_key().to_bytes(),
        not_before: 1,
        expires_at: 2,
    };
    settings
        .access
        .trusted_issuers
        .push(super::super::access::TrustedIssuer {
            root: URL_SAFE_NO_PAD.encode(generation.issuer),
            certificate: URL_SAFE_NO_PAD.encode(generation.certify(&root).unwrap()),
        });

    let effective_at = next_period(&node);
    let error = node.update(settings.clone(), effective_at).unwrap_err();
    assert!(format!("{error:#}").contains("injected issuer write failure"));
    let stored_budget = store.load_budgets().unwrap().monthly.unwrap();
    assert_eq!(stored_budget.period, previous_budget.period);
    assert_eq!(stored_budget.reserved, previous_budget.reserved);
    assert_public_settings(&node, &endpoint, OLD_CAPACITY, true);
    assert!(!node.available());
    assert!(!changes.has_changed().unwrap());

    db.execute_batch("DROP TRIGGER reject_issuer;").unwrap();
    node.update(settings, effective_at).unwrap();
    assert_eq!(
        store.load_budgets().unwrap().monthly.unwrap().period,
        effective_at
    );
    assert_public_settings(&node, &endpoint, NEW_CAPACITY, false);
    assert!(node.available());
    assert!(changes.has_changed().unwrap());
    assert_eq!(write_bytes(&node, QUOTA).unwrap(), QUOTA);
}

#[test]
fn rejected_file_allows_the_active_budget_to_roll_over() {
    let (directory, endpoint, node, mut settings) = configured_node();
    exhaust_budget(&node);
    change_public_settings(&mut settings);
    settings.relay_budget.as_mut().unwrap().reset_day_utc = 15;
    let path = directory.path().join("settings.json");
    std::fs::write(
        &path,
        serde_json::to_vec(&serde_json::json!({
            "listen": "127.0.0.1:8484",
            "database": "node.sqlite",
            "identity_file": "identity",
            "node": settings,
        }))
        .unwrap(),
    )
    .unwrap();

    assert!(
        node.refresh_settings(Some(&path), next_period(&node))
            .is_err()
    );
    assert_public_settings(&node, &endpoint, OLD_CAPACITY, true);
    assert!(node.available());
    assert_eq!(write_bytes(&node, QUOTA).unwrap(), QUOTA);
}

#[test]
fn unchanged_config_file_skips_reapplying_while_still_rolling_over_budgets() {
    let (directory, endpoint, node, settings) = configured_node();
    exhaust_budget(&node);
    let path = directory.path().join("settings.json");
    std::fs::write(
        &path,
        serde_json::to_vec(&serde_json::json!({
            "listen": "127.0.0.1:8484",
            "database": "node.sqlite",
            "identity_file": "identity",
            "node": settings,
        }))
        .unwrap(),
    )
    .unwrap();

    let current = now();
    node.refresh_settings(Some(&path), current).unwrap();
    let changes = node.access.changes();
    node.refresh_settings(Some(&path), current).unwrap();
    assert!(!changes.has_changed().unwrap());
    assert!(!node.available());

    // Crossing the billing period rolls over active budgets without re-applying unchanged settings.
    node.refresh_settings(Some(&path), next_period(&node))
        .unwrap();
    assert!(!changes.has_changed().unwrap());
    assert_public_settings(&node, &endpoint, OLD_CAPACITY, true);
    assert!(node.available());
    assert_eq!(write_bytes(&node, QUOTA).unwrap(), QUOTA);
}

#[test]
fn spent_public_day_changes_discovery_without_revoking_the_node() {
    let (_directory, endpoint, node, mut settings) = configured_node();
    settings.contribution.daily_bytes = Some(1);
    let today = now();
    node.update(settings.clone(), today).unwrap();
    let connection_epoch = node.epoch();
    assert_eq!(
        describe(&node, &endpoint)["public_relay"]["state"],
        "enabled"
    );
    assert_eq!(write_bytes(&node, 1).unwrap(), 1);
    assert_eq!(
        describe(&node, &endpoint)["public_relay"]["state"],
        "budget_exhausted"
    );
    assert!(node.available());
    node.update(settings, today + chrono::TimeDelta::days(1).num_seconds())
        .unwrap();
    assert_eq!(
        describe(&node, &endpoint)["public_relay"]["state"],
        "enabled"
    );
    assert_eq!(node.epoch(), connection_epoch);
}

fn capture_pacing_warnings(exercise: impl FnOnce()) -> Vec<serde_json::Value> {
    capture_events(exercise)
        .into_iter()
        .filter(|entry| entry["fields"]["event"] == "relay_pacing_disabled")
        .collect()
}

#[test]
fn unpaced_relay_warns_once_per_transition_including_default_startup() {
    const NO_WARNINGS: &[serde_json::Value] = &[];
    let directory = tempfile::tempdir().unwrap();
    let endpoint = Endpoint::parse("wss://node.example/v2/ws").unwrap();
    let store = Database::open(&directory.path().join("node.sqlite"), endpoint.identity).unwrap();
    let identity = Identity::generate().unwrap();
    let expected_node_id = identity.id();
    let node = Runtime::default();
    node.identify(identity).unwrap();

    let mut settings = Settings {
        services: super::super::Services {
            relay: false,
            stun: true,
        },
        ..Settings::default()
    };
    let warnings = capture_pacing_warnings(|| {
        node.install_settings(settings.clone(), store).unwrap();
    });
    assert_eq!(warnings, NO_WARNINGS);

    settings.services.relay = true;
    let warnings = capture_pacing_warnings(|| {
        node.update(settings.clone(), now()).unwrap();
        node.update(settings.clone(), now()).unwrap();
    });
    assert_eq!(warnings.len(), 1);
    assert_eq!(warnings[0]["fields"]["node_id"], expected_node_id);
    assert_eq!(warnings[0]["fields"]["zone"], "global");

    settings.ceiling.bytes_per_second = Some(OLD_CAPACITY);
    let warnings = capture_pacing_warnings(|| {
        node.update(settings.clone(), now()).unwrap();
    });
    assert_eq!(warnings, NO_WARNINGS);

    let invalid = Settings {
        ceiling: crate::node::Ceiling::default(),
        relay_budget: Some(MonthlyBudget {
            bytes: QUOTA as u64,
            reset_day_utc: 0,
        }),
        ..settings.clone()
    };
    let warnings = capture_pacing_warnings(|| {
        assert!(node.update(invalid, now()).is_err());
    });
    assert_eq!(warnings, NO_WARNINGS);

    // Public contribution pacing alone still governs non-owner traffic.
    settings.ceiling.bytes_per_second = None;
    settings.contribution.bytes_per_second = Some(NEW_CAPACITY);
    let warnings = capture_pacing_warnings(|| {
        node.update(settings.clone(), now()).unwrap();
    });
    assert_eq!(warnings, NO_WARNINGS);

    settings.contribution.bytes_per_second = None;
    let warnings = capture_pacing_warnings(|| {
        node.update(settings.clone(), now()).unwrap();
    });
    assert_eq!(warnings.len(), 1);

    let default_store =
        Database::open(&directory.path().join("default.sqlite"), endpoint.identity).unwrap();
    let default_node = Runtime::default();
    let warnings = capture_pacing_warnings(|| {
        default_node
            .install_settings(Settings::default(), default_store)
            .unwrap();
    });
    assert_eq!(warnings.len(), 1);
}
#[test]
fn headless_contribution_is_durable_and_independent_of_the_host_uplink() {
    use crate::node::{Ceiling, contribution::Policy};
    let dir = tempfile::tempdir().unwrap();
    let path = dir.path().join("node.sqlite");
    let endpoint = Endpoint::parse("wss://node.example/v2/ws").unwrap();
    let settings = Settings {
        ceiling: Ceiling {
            connections: Some(3),
            ..Default::default()
        },
        ..Settings::default()
    };
    let open = || {
        let node = Runtime::default();
        node.identify(Identity::generate().unwrap()).unwrap();
        node.configure(
            settings.clone(),
            Database::open(&path, endpoint.identity).unwrap(),
        )
        .unwrap();
        node
    };
    let node = open();
    let policy = Policy {
        enabled: false,
        host_uplink: Some(25_000_000),
        bytes_per_second: 2_000_000,
        ..Policy::default()
    };
    let changes = node.access.changes();
    assert!(node.set_contribution(policy, now()).unwrap());
    assert!(changes.has_changed().unwrap());
    assert_eq!(
        describe(&node, &endpoint)["public_relay"]["state"],
        "disabled"
    );
    assert!(!node.set_contribution(policy, now()).unwrap());
    drop(node);
    let node = open();
    assert_eq!(
        describe(&node, &endpoint)["public_relay"]["state"],
        "disabled"
    );
    assert!(describe(&node, &endpoint)["public_capacity_bytes_per_second"].is_null());
    assert_eq!(node.access.public_connections(), 3);
    {
        let state = node.state.lock().unwrap();
        assert_eq!(state.settings.contribution.daily_bytes, None);
        assert_eq!(state.settings.contribution.bytes_per_second, None);
        assert_eq!(state.settings.host_uplink, None);
    }
    assert!(
        !node
            .set_contribution(
                Policy {
                    enabled: true,
                    daily_bytes: 0,
                    ..policy
                },
                now(),
            )
            .unwrap()
    );
    assert_eq!(
        describe(&node, &endpoint)["public_capacity_bytes_per_second"],
        2_000_000
    );
    assert_eq!(
        describe(&node, &endpoint)["public_relay"]["state"],
        "budget_exhausted"
    );
}

#[test]
fn contribution_updates_apply_synchronously_even_when_config_file_is_broken() {
    use crate::node::{Ceiling, contribution::Policy};
    let dir = tempfile::tempdir().unwrap();
    let db_path = dir.path().join("node.sqlite");
    let config_path = dir.path().join("node.json");
    let endpoint = Endpoint::parse("wss://node.example/v2/ws").unwrap();
    let initial = Settings {
        ceiling: Ceiling {
            bytes_per_second: Some(5_000_000),
            connections: Some(8),
            ..Default::default()
        },
        relay_budget: Some(MonthlyBudget {
            bytes: QUOTA as u64,
            reset_day_utc: 1,
        }),
        ..Settings::default()
    };
    let node = Runtime::default();
    node.identify(Identity::generate().unwrap()).unwrap();
    node.configure(
        initial.clone(),
        Database::open(&db_path, endpoint.identity).unwrap(),
    )
    .unwrap();

    // Corrupt node.json before saving contribution limits: console updates must still apply.
    std::fs::write(&config_path, b"{not valid json").unwrap();
    assert!(node.refresh_settings(Some(&config_path), now()).is_err());

    let updated = Policy {
        enabled: false,
        daily_bytes: 4_096,
        bytes_per_second: 10_000_000,
        connections: 32,
        host_uplink: Some(25_000_000),
    };
    assert!(node.set_contribution(updated, now()).unwrap());
    let descriptor = describe(&node, &endpoint);
    assert_eq!(descriptor["public_relay"]["state"], "disabled");
    assert_eq!(descriptor["public_capacity_bytes_per_second"], 5_000_000);
    assert_eq!(node.access.public_connections(), 8);

    // A rejected node.json reload must not overwrite the ceiling reported to the console.
    let mut rejected = initial.clone();
    rejected.ceiling.connections = Some(2);
    rejected.relay_budget.as_mut().unwrap().reset_day_utc = 15;
    std::fs::write(
        &config_path,
        serde_json::to_vec(&serde_json::json!({
            "listen": "127.0.0.1:8484",
            "database": "node.sqlite",
            "identity_file": "identity",
            "node": rejected,
        }))
        .unwrap(),
    )
    .unwrap();
    assert!(node.refresh_settings(Some(&config_path), now()).is_err());
    assert_eq!(
        node.contribution_view().unwrap()["ceiling"]["connections"],
        8
    );

    // Once node.json recovers with a tighter ceiling, the saved contribution policy survives.
    let mut recovered = initial;
    recovered.ceiling.bytes_per_second = Some(1_500_000);
    recovered.ceiling.connections = Some(4);
    std::fs::write(
        &config_path,
        serde_json::to_vec(&serde_json::json!({
            "listen": "127.0.0.1:8484",
            "database": "node.sqlite",
            "identity_file": "identity",
            "node": recovered,
        }))
        .unwrap(),
    )
    .unwrap();
    node.refresh_settings(Some(&config_path), now()).unwrap();
    let descriptor = describe(&node, &endpoint);
    assert_eq!(descriptor["public_relay"]["state"], "disabled");
    assert_eq!(descriptor["public_capacity_bytes_per_second"], 1_500_000);
    assert_eq!(node.access.public_connections(), 4);
    assert_eq!(
        node.contribution_view().unwrap()["ceiling"]["connections"],
        4
    );
}

#[test]
fn disabling_contribution_clears_community_caps_when_no_static_ceiling_is_configured() {
    use crate::node::contribution::Policy;
    let dir = tempfile::tempdir().unwrap();
    let endpoint = Endpoint::parse("wss://node.example/v2/ws").unwrap();
    let store = Database::open(&dir.path().join("node.sqlite"), endpoint.identity).unwrap();
    let node = Runtime::default();
    node.configure(Settings::default(), store).unwrap();
    assert!(node.access.public_connections() < usize::MAX);
    assert!(
        node.set_contribution(
            Policy {
                enabled: false,
                daily_bytes: 0,
                bytes_per_second: 1_000_000,
                connections: 4,
                host_uplink: Some(10_000_000),
            },
            now(),
        )
        .unwrap()
    );
    assert_eq!(node.access.public_connections(), usize::MAX);
    let state = node.state.lock().unwrap();
    assert_eq!(state.settings.contribution.daily_bytes, None);
    assert_eq!(state.settings.contribution.bytes_per_second, None);
    assert_eq!(state.settings.host_uplink, None);
    assert!(!state.settings.access.contributing);
    drop(state);
}

#[test]
fn bootstrap_nodes_hot_reload_updates_subscribers_and_rejects_reserved_suffixes() {
    let dir = tempfile::tempdir().unwrap();
    let db_path = dir.path().join("node.sqlite");
    let config_path = dir.path().join("node.json");
    let endpoint = Endpoint::parse("wss://node.example/v2/ws").unwrap();

    let mut settings = Settings {
        bootstrap_nodes: vec!["https://seed1.example".into()],
        ..Settings::default()
    };
    let node = Runtime::default();
    node.identify(Identity::generate().unwrap()).unwrap();
    node.configure(
        settings.clone(),
        Database::open(&db_path, endpoint.identity).unwrap(),
    )
    .unwrap();

    let mut seeds_watch = node.subscribe_bootstrap_nodes();
    assert_eq!(
        seeds_watch.borrow_and_update().as_ref(),
        ["https://seed1.example"]
    );

    let write_config = |node_settings: &Settings| {
        std::fs::write(
            &config_path,
            serde_json::to_vec(&serde_json::json!({
                "listen": "127.0.0.1:8484",
                "database": "node.sqlite",
                "identity_file": "identity",
                "node": node_settings,
            }))
            .unwrap(),
        )
        .unwrap();
    };

    // Unrelated node edits must not wake bootstrap_nodes subscribers.
    settings.region = "us-west".into();
    write_config(&settings);
    node.refresh_settings(Some(&config_path), now()).unwrap();
    assert!(!seeds_watch.has_changed().unwrap());

    // Valid bootstrap_nodes updates notify subscribers and update Runtime::bootstrap_nodes.
    settings
        .bootstrap_nodes
        .push("https://seed2.example".into());
    write_config(&settings);
    node.refresh_settings(Some(&config_path), now()).unwrap();
    assert!(seeds_watch.has_changed().unwrap());
    assert_eq!(
        seeds_watch.borrow_and_update().as_ref(),
        ["https://seed1.example", "https://seed2.example"]
    );

    // Reserved DNS suffix seeds fail reload without disturbing active seeds.
    let mut invalid = settings;
    invalid.bootstrap_nodes = vec!["https://seed.local".into()];
    write_config(&invalid);
    assert!(node.refresh_settings(Some(&config_path), now()).is_err());
    assert!(!seeds_watch.has_changed().unwrap());
    assert_eq!(
        seeds_watch.borrow().as_ref(),
        ["https://seed1.example", "https://seed2.example"]
    );
}
