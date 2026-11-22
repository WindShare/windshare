use super::*;
use crate::{
    config::limits,
    environment::Trace,
    test_support::{Fixture, connection, work},
};
use windshare_relay_core::share::{Control, Input, Output, ShareRelay, WorkResult};

fn session_keys<const N: usize>() -> [SessionKey; N] {
    let fixture = Fixture::new();
    let at = fixture.now(false);
    let (mut relay, _) =
        ShareRelay::restore(fixture.identity(), limits(), None, Trace, at).unwrap();
    fixture.begin(&mut relay, false);
    let _ = relay.handle(
        Input::Control {
            connection: connection(1),
            frame: &fixture.bytes("registerProofB64"),
        },
        at,
    );
    let publication = work(relay.handle(
        Input::Control {
            connection: connection(1),
            frame: &fixture.bytes("descriptorUploadB64"),
        },
        at,
    ));
    let _ = relay.handle(
        Input::Completed {
            id: publication.id,
            result: WorkResult::Complete,
        },
        at,
    );
    let join = fixture.bytes("joinB64");
    std::array::from_fn(|index| {
        let receiver = connection(index as u64 + 2);
        let _ = relay.handle(Input::Connected(receiver), at);
        let step = relay.handle(
            Input::Control {
                connection: receiver,
                frame: &join,
            },
            at,
        );
        step.outputs
            .into_iter()
            .find_map(|output| match output {
                Output::Send {
                    control: Control::Descriptor { session, .. },
                    ..
                } => Some(session),
                _ => None,
            })
            .expect("admitted session key")
    })
}

#[test]
fn session_lifecycle_merges_overlapping_intervals_and_buckets_throughput() {
    let metrics = Metrics::default();
    let [key] = session_keys();
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
    let [low, mid, idle, instant] = session_keys();
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
    let [unknown] = session_keys();
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
    let [first, second] = session_keys();
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
