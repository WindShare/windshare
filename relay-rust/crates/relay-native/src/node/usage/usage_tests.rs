use super::*;
use windshare_relay_access::Attribution;

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

#[test]
fn persisted_batches_retain_ids_and_bytes_across_process_restart() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("usage.db");
    let relay = crate::Endpoint::parse("ws://localhost:8787/v2/ws")
        .unwrap()
        .identity;
    let store = crate::Database::open(&path, relay).unwrap();
    store.initialize_control().unwrap();
    let batch = windshare_relay_access::usage::Batch {
        id: [1; 16],
        node: [2; 32],
        issuer: [3; 32],
        created_at: DAY_SECONDS,
        records: vec![windshare_relay_access::usage::UsageRecord {
            account: [4; 16],
            day: 0,
            bytes: 17,
        }],
        resident_accounts: vec![],
        shares: vec![],
        load: windshare_relay_access::usage::Load {
            active_sessions: 0,
            wire_bytes: 17,
            forwarded_bytes: 17,
            admission_refusals: 0,
        },
    };
    store.enqueue_usage(std::slice::from_ref(&batch)).unwrap();
    drop(store);
    let store = crate::Database::open(&path, relay).unwrap();
    store.initialize_control().unwrap();
    assert_eq!(store.pending_usage([3; 32]).unwrap(), Some(batch.clone()));
    store.acknowledge_usage(batch.id).unwrap();
    assert!(store.pending_usage([3; 32]).unwrap().is_none());
}
