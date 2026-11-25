use super::*;
use crate::Endpoint;
use std::path::Path;
use windshare_relay_access::{
    entitlement::DAY_SECONDS,
    usage::{Load, UsageRecord},
};

const ISSUER_A: Issuer = [1; 32];
const ISSUER_B: Issuer = [2; 32];
const NODE: [u8; 32] = [3; 32];

fn open(path: &Path) -> Database {
    let relay = Endpoint::parse("ws://localhost:8787/v2/ws")
        .unwrap()
        .identity;
    Database::open(path, relay).unwrap()
}

fn batch(issuer: Issuer, id: u8, created_at: u64) -> Batch {
    Batch {
        id: [id; 16],
        node: NODE,
        issuer,
        created_at,
        records: vec![UsageRecord {
            account: [4; 16],
            day: created_at / DAY_SECONDS,
            bytes: 17,
        }],
        resident_accounts: vec![],
        load: Load {
            relay_budget: None,
            applied_policy_version: 0,
            resident_shares: 0,
            active_sessions: 0,
            wire_bytes: 17,
            forwarded_bytes: 17,
            admission_refusals: 0,
        },
    }
}

fn query_plan(db: &Database, issuer: Issuer) -> Vec<String> {
    let connection = db.0.connection.lock().unwrap();
    let sql = format!("EXPLAIN QUERY PLAN {NEXT_USAGE_BATCH_SQL}");
    let mut statement = connection.prepare(&sql).unwrap();
    let details = statement
        .query_map([issuer.as_slice()], |row| row.get::<_, String>(3))
        .unwrap()
        .collect::<rusqlite::Result<Vec<_>>>()
        .unwrap();
    drop(statement);
    drop(connection);
    details
}

fn assert_indexed_queue_plan(plan: &[String]) {
    let summary = plan.join("\n");
    assert!(
        summary.contains("USING INDEX usage_outbox_queue"),
        "expected queue index in query plan, got:\n{summary}"
    );
    assert!(
        !summary.contains("SCAN usage_outbox") && !summary.contains("TEMP B-TREE"),
        "expected no table scan or temporary sort, got:\n{summary}"
    );
}

#[test]
fn next_usage_batch_uses_queue_index_and_preserves_fifo_order() {
    let directory = tempfile::tempdir().unwrap();
    let db = open(&directory.path().join("node.db"));
    db.initialize_control(0).unwrap();

    let other_issuer = batch(ISSUER_B, 1, DAY_SECONDS);
    let later = batch(ISSUER_A, 4, DAY_SECONDS + 120);
    let tie_high = batch(ISSUER_A, 3, DAY_SECONDS + 60);
    let tie_low = batch(ISSUER_A, 2, DAY_SECONDS + 60);
    db.enqueue_usage(&[
        other_issuer.clone(),
        later.clone(),
        tie_high.clone(),
        tie_low.clone(),
    ])
    .unwrap();

    assert_indexed_queue_plan(&query_plan(&db, ISSUER_A));

    for expected in [&tie_low, &tie_high, &later] {
        let next = db.next_usage_batch(ISSUER_A, NODE).unwrap().unwrap();
        assert_eq!(next, *expected);
        db.acknowledge_usage(next.id).unwrap();
    }
    assert!(db.next_usage_batch(ISSUER_A, NODE).unwrap().is_none());
    assert_eq!(
        db.next_usage_batch(ISSUER_B, NODE).unwrap(),
        Some(other_issuer)
    );
}

#[test]
fn initialize_control_creates_queue_index_for_existing_outbox() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("legacy.db");
    let existing = batch(ISSUER_A, 9, DAY_SECONDS + 30);

    let db = open(&path);
    {
        let connection = db.0.connection.lock().unwrap();
        connection
            .execute_batch(
                "CREATE TABLE usage_outbox(
                    id BLOB PRIMARY KEY, issuer BLOB NOT NULL, body BLOB NOT NULL,
                    created_at INTEGER NOT NULL) STRICT;",
            )
            .unwrap();
        connection
            .execute(
                "INSERT INTO usage_outbox VALUES(?1,?2,?3,?4)",
                params![
                    existing.id.as_slice(),
                    existing.issuer.as_slice(),
                    serde_json::to_vec(&existing).unwrap(),
                    i64::try_from(existing.created_at).unwrap()
                ],
            )
            .unwrap();
        drop(connection);
    }
    drop(db);

    let db = open(&path);
    db.initialize_control(DAY_SECONDS + 30).unwrap();
    assert_indexed_queue_plan(&query_plan(&db, ISSUER_A));
    assert_eq!(db.next_usage_batch(ISSUER_A, NODE).unwrap(), Some(existing));
}
