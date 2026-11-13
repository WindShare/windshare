use serde::Serialize;
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};
use windshare_relay_core::{identifiers::ShareId, sessions::SessionKey};

const THROUGHPUT_BOUNDS: [u64; 6] = [64_000, 256_000, 1_000_000, 4_000_000, 16_000_000, u64::MAX];

#[derive(Clone, Debug, Default, Serialize)]
pub struct Snapshot {
    pub active_sessions: u64,
    pub forwarded_bytes: u64,
    pub wire_bytes: u64,
    pub forwarding_nanoseconds: u64,
    pub completed_sessions: u64,
    pub admission_refusals: u64,
    pub queue_refusals: u64,
    pub throughput_bytes_per_second_bounds: [u64; 6],
    pub throughput_sessions: [u64; 6],
}

struct Session {
    key: SessionKey,
    bytes: u64,
    active: Duration,
    last_end: Instant,
}

#[derive(Default)]
struct State {
    sessions: BTreeMap<ShareId, Vec<Session>>,
    totals: Snapshot,
}

#[derive(Clone, Default)]
pub struct Metrics(Arc<Mutex<State>>);

impl Metrics {
    pub fn begin(&self, key: SessionKey) {
        let mut state = self.0.lock().expect("node metrics");
        let sessions = state.sessions.entry(key.share()).or_default();
        if sessions.iter().any(|entry| entry.key == key) {
            return;
        }
        sessions.push(Session {
            key,
            bytes: 0,
            active: Duration::ZERO,
            last_end: Instant::now(),
        });
        state.totals.active_sessions += 1;
    }

    pub fn end(&self, key: SessionKey) {
        let mut state = self.0.lock().expect("node metrics");
        let Some(sessions) = state.sessions.get_mut(&key.share()) else {
            return;
        };
        let Some(index) = sessions.iter().position(|entry| entry.key == key) else {
            return;
        };
        let session = sessions.swap_remove(index);
        if sessions.is_empty() {
            state.sessions.remove(&key.share());
        }
        state.totals.active_sessions -= 1;
        state.totals.completed_sessions += 1;
        if session.bytes > 0 {
            let rate = (u128::from(session.bytes) * 1_000_000_000
                / session.active.as_nanos().max(1))
            .min(u128::from(u64::MAX));
            let bucket = THROUGHPUT_BOUNDS
                .iter()
                .position(|bound| rate <= u128::from(*bound))
                .expect("unbounded bucket");
            state.totals.throughput_sessions[bucket] += 1;
        }
    }

    pub fn forwarded(&self, key: SessionKey, bytes: u64, start: Instant, end: Instant) {
        let mut state = self.0.lock().expect("node metrics");
        state.totals.forwarded_bytes = state.totals.forwarded_bytes.saturating_add(bytes);
        if let Some(session) = state
            .sessions
            .get_mut(&key.share())
            .and_then(|sessions| sessions.iter_mut().find(|entry| entry.key == key))
        {
            // Concurrent directions belong to one session's active-time union.
            let active = end.saturating_duration_since(start.max(session.last_end));
            session.active += active;
            session.bytes = session.bytes.saturating_add(bytes);
            session.last_end = session.last_end.max(end);
            state.totals.forwarding_nanoseconds = state
                .totals
                .forwarding_nanoseconds
                .saturating_add(u64::try_from(active.as_nanos()).unwrap_or(u64::MAX));
        }
    }

    pub fn wire(&self, bytes: usize) {
        let mut state = self.0.lock().expect("node metrics");
        state.totals.wire_bytes = state.totals.wire_bytes.saturating_add(bytes as u64);
    }

    pub fn admission_refused(&self) {
        self.0
            .lock()
            .expect("node metrics")
            .totals
            .admission_refusals += 1;
    }

    pub fn queue_refused(&self) {
        self.0.lock().expect("node metrics").totals.queue_refusals += 1;
    }

    pub fn snapshot(&self) -> Snapshot {
        let mut snapshot = self.0.lock().expect("node metrics").totals.clone();
        snapshot.throughput_bytes_per_second_bounds = THROUGHPUT_BOUNDS;
        snapshot
    }
}
