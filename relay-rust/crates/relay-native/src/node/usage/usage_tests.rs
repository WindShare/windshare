use super::*;
use crate::{
    Database, Endpoint,
    node::{Identity, Runtime},
};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use std::path::Path;
use windshare_relay_access::{
    Attribution,
    usage::{Batch, Load, UsageRecord},
};

fn usage_store(path: &Path) -> Database {
    let relay = Endpoint::parse("ws://localhost:8787/v2/ws")
        .unwrap()
        .identity;
    let store = Database::open(path, relay).unwrap();
    store.initialize_control().unwrap();
    store
}

#[test]
fn only_actual_writes_in_a_forwarding_scope_count_and_midnight_splits_days() {
    let collector = Collector::default();
    collector.configure([1; 32]);
    let meter = Meter::default();
    let account = Attribution::Account {
        issuer: [1; 32],
        account: [2; 16],
        credential: [3; 16],
    };
    meter.record(&collector, 1000, DAY_SECONDS - 1);
    let scope = meter.enter(account);
    meter.record(&collector, 7, DAY_SECONDS - 1);
    meter.record(&collector, 9, DAY_SECONDS);
    drop(scope);
    meter.record(&collector, 2000, DAY_SECONDS);
    let records = collector.drain();
    assert_eq!(records.len(), 2);
    assert_eq!(records[&([1; 32], [2; 16], 0)], 7);
    assert_eq!(records[&([1; 32], [2; 16], 1)], 9);
    collector.restore(records.clone());
    assert_eq!(collector.drain(), records);
}

#[tokio::test]
async fn flush_reads_the_clock_after_draining_midnight_usage() {
    let directory = tempfile::tempdir().unwrap();
    let store = usage_store(&directory.path().join("usage.db"));
    let issuer = [1; 32];
    let account = [2; 16];
    let reporter = Reporter::new(
        Runtime::default(),
        store.clone(),
        Identity::generate().unwrap(),
        Authority {
            url: "http://localhost".into(),
            issuer: URL_SAFE_NO_PAD.encode(issuer),
        },
    )
    .unwrap();
    let collector = &reporter.node.usage;
    let attribution = Attribution::Account {
        issuer,
        account,
        credential: [3; 16],
    };
    collector.record(attribution, 7, DAY_SECONDS - 1);
    collector.record(attribution, 9, DAY_SECONDS);

    reporter
        .flush(|| {
            collector.record(attribution, 11, DAY_SECONDS);
            i64::try_from(DAY_SECONDS).unwrap()
        })
        .await
        .unwrap();

    let batch = store.next_usage_batch(issuer).unwrap().unwrap();
    assert_eq!(batch.created_at, DAY_SECONDS);
    assert_eq!(
        batch.records,
        [
            UsageRecord {
                account,
                day: 0,
                bytes: 7
            },
            UsageRecord {
                account,
                day: 1,
                bytes: 9
            },
        ]
    );
    assert_eq!(
        collector.drain(),
        BTreeMap::from([((issuer, account, 1), 11)])
    );
}

fn usage_batch() -> Batch {
    Batch {
        id: [1; 16],
        node: [2; 32],
        issuer: [3; 32],
        created_at: DAY_SECONDS,
        records: vec![UsageRecord {
            account: [4; 16],
            day: 0,
            bytes: 17,
        }],
        resident_accounts: vec![],
        shares: vec![],
        load: Load {
            active_sessions: 0,
            wire_bytes: 17,
            forwarded_bytes: 17,
            admission_refusals: 0,
        },
    }
}

#[test]
fn persisted_batches_retain_ids_and_bytes_across_process_restart() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("usage.db");
    let store = usage_store(&path);
    let batch = usage_batch();
    store.enqueue_usage(std::slice::from_ref(&batch)).unwrap();
    drop(store);
    let store = usage_store(&path);
    assert_eq!(
        store.next_usage_batch([3; 32]).unwrap(),
        Some(batch.clone())
    );
    store.acknowledge_usage(batch.id).unwrap();
    assert!(store.next_usage_batch([3; 32]).unwrap().is_none());
}

#[test]
fn invalid_outbox_entries_are_preserved_without_blocking_the_next_batch() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("usage.db");
    let store = usage_store(&path);
    let good = usage_batch();
    let mut invalid = good.clone();
    invalid.id = [2; 16];
    invalid.created_at = DAY_SECONDS - 1;
    invalid.records[0].day = 1;

    assert!(
        store
            .enqueue_usage(&[good.clone(), invalid.clone()])
            .is_err()
    );
    assert!(store.next_usage_batch(good.issuer).unwrap().is_none());
    store.enqueue_usage(std::slice::from_ref(&good)).unwrap();

    // Seed a pre-fix batch and a damaged row outside the checked enqueue path.
    let entries = [
        (
            invalid.id,
            serde_json::to_vec(&invalid).unwrap(),
            "invalid_usage_batch",
        ),
        ([3; 16], b"{".to_vec(), "invalid_json"),
    ];
    let created_at = i64::try_from(invalid.created_at).unwrap();
    let sql = rusqlite::Connection::open(&path).unwrap();
    for (id, body, _) in &entries {
        sql.execute(
            "INSERT INTO usage_outbox VALUES(?1,?2,?3,?4)",
            rusqlite::params![id.as_slice(), good.issuer.as_slice(), body, created_at],
        )
        .unwrap();
    }
    drop(sql);

    assert_eq!(
        store.next_usage_batch(good.issuer).unwrap().as_ref(),
        Some(&good)
    );
    drop(store);

    let store = usage_store(&path);
    assert_eq!(
        store.next_usage_batch(good.issuer).unwrap().as_ref(),
        Some(&good)
    );
    store.acknowledge_usage(good.id).unwrap();
    assert!(store.next_usage_batch(good.issuer).unwrap().is_none());

    let sql = rusqlite::Connection::open(&path).unwrap();
    for (id, body, reason) in entries {
        let saved: (Issuer, Vec<u8>, i64, String) = sql
            .query_row(
                "SELECT issuer,body,created_at,reason FROM usage_outbox_quarantine WHERE id=?1",
                [id.as_slice()],
                |row| Ok((row.get(0)?, row.get(1)?, row.get(2)?, row.get(3)?)),
            )
            .unwrap();
        assert_eq!(saved, (good.issuer, body, created_at, reason.to_owned()));
    }
}

#[test]
fn persisted_restrictions_migrate_once_and_survive_other_control_writes() {
    use crate::node::access::Cache;
    use windshare_relay_access::restrictions::{Restriction, Restrictions};

    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("usage.db");
    let relay = crate::Endpoint::parse("ws://localhost:8787/v2/ws")
        .unwrap()
        .identity;
    let store = crate::Database::open(&path, relay).unwrap();
    store.initialize_control().unwrap();
    let issuer = [3; 32];
    let mut previous = Restrictions {
        version: 255,
        ..Restrictions::default()
    };
    previous.insert(Restriction::Credential([4; 16]));
    store
        .save_access(Cache {
            restrictions: vec![(issuer, previous.clone())],
            ..Cache::default()
        })
        .unwrap();
    assert_eq!(
        store.initialize_control().unwrap().restrictions,
        vec![(issuer, previous.clone())]
    );
    assert_eq!(
        store.save_access(Cache::default()).unwrap().restrictions,
        []
    );

    let mut current = previous.clone();
    current.version += 1;
    current.insert(Restriction::Share([5; 12]));
    store.save_restrictions(issuer, &current).unwrap();
    store.save_restrictions(issuer, &previous).unwrap();
    drop(store);

    let store = crate::Database::open(&path, relay).unwrap();
    assert_eq!(
        store.initialize_control().unwrap().restrictions,
        vec![(issuer, current)]
    );
}
