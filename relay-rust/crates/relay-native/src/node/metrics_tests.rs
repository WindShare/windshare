use super::*;
use windshare_relay_core::sessions::SessionSnapshot;

fn session_key(share: u8, session_id: u64) -> SessionKey {
    const FORMAT: u8 = 1;
    const ACTIVE: u8 = 3;

    let mut encoded = Vec::new();
    encoded.extend_from_slice(&[FORMAT, ACTIVE]);
    encoded.extend_from_slice(&[1; 32]);
    encoded.extend_from_slice(&[share; 12]);
    encoded.extend_from_slice(&[1; 16]);
    encoded.extend_from_slice(&session_id.to_be_bytes());
    encoded.extend_from_slice(&1_u64.to_be_bytes());
    // Sender ConnectionRef: big-endian (id, generation).
    encoded.extend_from_slice(&1_u64.to_be_bytes());
    encoded.extend_from_slice(&1_u64.to_be_bytes());
    encoded.extend_from_slice(&[1; 16]);
    // Receiver ConnectionRef: big-endian (id, generation).
    encoded.extend_from_slice(&(session_id + 1).to_be_bytes());
    encoded.extend_from_slice(&1_u64.to_be_bytes());
    encoded.extend_from_slice(&session_id.to_be_bytes());

    SessionSnapshot::decode(&encoded)
        .expect("valid active session snapshot")
        .key()
}

#[test]
fn session_lifecycle_merges_overlapping_intervals_and_buckets_throughput() {
    let metrics = Metrics::default();
    let key = session_key(1, 1);
    let base = Instant::now();

    metrics.begin(key);
    metrics.begin(key);
    assert_eq!(metrics.snapshot().active_sessions, 1);

    // Overlapping [0..100ms] + [40..120ms], nested [50..80ms], disjoint [200..280ms] => 200ms.
    metrics.forwarded(key, 50_000, base, base + Duration::from_millis(100));
    metrics.forwarded(
        key,
        50_000,
        base + Duration::from_millis(40),
        base + Duration::from_millis(120),
    );
    metrics.forwarded(
        key,
        20_000,
        base + Duration::from_millis(50),
        base + Duration::from_millis(80),
    );
    metrics.forwarded(
        key,
        80_000,
        base + Duration::from_millis(200),
        base + Duration::from_millis(280),
    );

    metrics.end(key);
    metrics.end(key);

    let snapshot = metrics.snapshot();
    assert_eq!(snapshot.active_sessions, 0);
    assert_eq!(snapshot.completed_sessions, 1);
    assert_eq!(snapshot.forwarded_bytes, 200_000);
    assert_eq!(snapshot.forwarding_nanoseconds, 200_000_000);
    assert_eq!(
        snapshot.throughput_bytes_per_second_bounds,
        THROUGHPUT_BOUNDS
    );
    assert_eq!(snapshot.throughput_sessions, [0, 0, 1, 0, 0, 0]);
}

#[test]
fn sibling_sessions_track_independently_and_handle_edge_rates() {
    let metrics = Metrics::default();
    let low = session_key(2, 1);
    let mid = session_key(2, 2);
    let idle = session_key(2, 3);
    let instant = session_key(2, 4);
    let base = Instant::now();

    for key in [low, mid, idle, instant] {
        metrics.begin(key);
    }
    assert_eq!(metrics.snapshot().active_sessions, 4);

    metrics.forwarded(low, 64_000, base, base + Duration::from_secs(1));
    metrics.forwarded(mid, 64_001, base, base + Duration::from_secs(1));
    metrics.forwarded(instant, 64, base, base);

    metrics.end(low);
    assert_eq!(metrics.snapshot().active_sessions, 3);
    metrics.end(mid);
    metrics.end(idle);
    metrics.end(instant);

    let snapshot = metrics.snapshot();
    assert_eq!(snapshot.active_sessions, 0);
    assert_eq!(snapshot.completed_sessions, 4);
    assert_eq!(snapshot.forwarded_bytes, 64_000 + 64_001 + 64);
    assert_eq!(snapshot.forwarding_nanoseconds, 2_000_000_000);
    assert_eq!(snapshot.throughput_sessions, [1, 1, 0, 0, 0, 1]);
}

#[test]
fn wire_refusals_and_untracked_forwards_accumulate_independently() {
    let metrics = Metrics::default();
    let unknown = session_key(3, 1);
    let base = Instant::now();

    metrics.wire(128);
    metrics.wire(64);
    metrics.admission_refused();
    metrics.admission_refused();
    metrics.queue_refused();
    metrics.forwarded(unknown, 4_096, base, base + Duration::from_millis(50));
    metrics.end(unknown);

    let snapshot = metrics.snapshot();
    assert_eq!(snapshot.wire_bytes, 192);
    assert_eq!(snapshot.admission_refusals, 2);
    assert_eq!(snapshot.queue_refusals, 1);
    assert_eq!(snapshot.forwarded_bytes, 4_096);
    assert_eq!(snapshot.forwarding_nanoseconds, 0);
    assert_eq!(snapshot.active_sessions, 0);
    assert_eq!(snapshot.completed_sessions, 0);
    assert_eq!(snapshot.throughput_sessions, [0; 6]);
}

#[test]
fn forwarding_recorder_bypasses_global_session_lock_and_evicts_ended_handles() {
    let metrics = Metrics::default();
    let first = session_key(4, 1);
    let second = session_key(4, 2);
    let base = Instant::now();

    metrics.begin(first);
    let mut outbound = metrics.recorder();
    let mut inbound = metrics.recorder();

    // First write on each direction resolves and caches the shared per-session handle.
    outbound.forwarded(first, 50_000, base, base + Duration::from_millis(100));
    inbound.forwarded(
        first,
        50_000,
        base + Duration::from_millis(40),
        base + Duration::from_millis(120),
    );

    // Steady-state writes must succeed while another thread holds the node-wide session write lock.
    let exclusive = metrics.0.sessions.write().expect("node metrics");
    outbound.forwarded(
        first,
        100_000,
        base + Duration::from_millis(120),
        base + Duration::from_millis(200),
    );
    drop(exclusive);

    metrics.end(first);
    // A trailing write for an ended session still counts forwarded_bytes but drops its cached handle.
    outbound.forwarded(
        first,
        1_024,
        base + Duration::from_millis(200),
        base + Duration::from_millis(250),
    );
    assert!(outbound.sessions.is_empty());

    // Opening a subsequent session on a long-lived writer prunes ended handles cached on `inbound`.
    metrics.begin(second);
    inbound.forwarded(second, 64_000, base, base + Duration::from_secs(1));
    assert_eq!(inbound.sessions.len(), 1);
    assert_eq!(inbound.sessions[0].key, second);
    metrics.end(second);

    let snapshot = metrics.snapshot();
    assert_eq!(snapshot.active_sessions, 0);
    assert_eq!(snapshot.completed_sessions, 2);
    assert_eq!(snapshot.forwarded_bytes, 200_000 + 1_024 + 64_000);
    assert_eq!(snapshot.forwarding_nanoseconds, 1_200_000_000);
    assert_eq!(snapshot.throughput_sessions, [1, 0, 1, 0, 0, 0]);
}
