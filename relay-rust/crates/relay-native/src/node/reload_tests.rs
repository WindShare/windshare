use super::*;
use crate::{Database, Endpoint, node::MonthlyBudget};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use chrono::{DateTime, Months};
use ed25519_dalek::SigningKey;
use rusqlite::Connection;
use tempfile::TempDir;
use windshare_relay_access::trust::Generation;

const OLD_CAPACITY: u64 = 10_000_000;
const NEW_CAPACITY: u64 = 2_000_000;
const QUOTA: u64 = 128;

fn configured_node() -> (TempDir, Endpoint, Runtime, Settings) {
    let directory = tempfile::tempdir().unwrap();
    let endpoint = Endpoint::parse("wss://node.example/v2/ws").unwrap();
    let store = Database::open(&directory.path().join("node.sqlite"), endpoint.identity).unwrap();
    let settings = Settings {
        capacity_bytes_per_second: Some(OLD_CAPACITY),
        relay_budget: Some(MonthlyBudget {
            bytes: QUOTA,
            reset_day_utc: 1,
        }),
        ..Settings::default()
    };
    let node = Runtime::default();
    node.identify(Identity::generate().unwrap()).unwrap();
    node.configure(settings.clone(), store).unwrap();
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
    assert_eq!(descriptor["capacity_bytes_per_second"], capacity);
    assert_eq!(descriptor["anonymous"], anonymous);
}

fn next_period(node: &Runtime) -> i64 {
    let period = node.state.lock().unwrap().budget.as_ref().unwrap().period();
    DateTime::from_timestamp(period, 0)
        .unwrap()
        .checked_add_months(Months::new(1))
        .unwrap()
        .timestamp()
}

fn change_public_settings(settings: &mut Settings) {
    settings.capacity_bytes_per_second = Some(NEW_CAPACITY);
    settings.access.anonymous = false;
}

fn exhaust_budget(node: &Runtime) {
    assert_eq!(node.reserve_for_period(QUOTA).unwrap().bytes, QUOTA);
    assert_eq!(node.reserve_for_period(1).unwrap().bytes, 0);
    assert!(!node.available());
}

#[test]
fn rejected_reset_day_keeps_live_settings_and_descriptor() {
    let (_directory, endpoint, node, mut settings) = configured_node();
    let changes = node.access.changes();
    let previous_epoch = node.budget_epoch();
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
    assert!(node.budget_epoch() == previous_epoch);
}

#[test]
fn issuer_write_failure_rolls_back_the_pending_billing_period() {
    let (directory, endpoint, node, mut settings) = configured_node();
    exhaust_budget(&node);
    let changes = node.access.changes();
    let store = node.state.lock().unwrap().control.clone().unwrap();
    let previous_budget = store.load_budget().unwrap().unwrap();

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
    let stored_budget = store.load_budget().unwrap().unwrap();
    assert_eq!(stored_budget.period, previous_budget.period);
    assert_eq!(stored_budget.reserved, previous_budget.reserved);
    assert_public_settings(&node, &endpoint, OLD_CAPACITY, true);
    assert!(!node.available());
    assert!(!changes.has_changed().unwrap());

    db.execute_batch("DROP TRIGGER reject_issuer;").unwrap();
    node.update(settings, effective_at).unwrap();
    assert_eq!(store.load_budget().unwrap().unwrap().period, effective_at);
    assert_public_settings(&node, &endpoint, NEW_CAPACITY, false);
    assert!(node.available());
    assert!(changes.has_changed().unwrap());
    assert_eq!(node.reserve_for_period(QUOTA).unwrap().bytes, QUOTA);
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
    assert_eq!(node.reserve_for_period(QUOTA).unwrap().bytes, QUOTA);
}
