use serde::Serialize;
use std::{
    collections::BTreeMap,
    sync::{
        Arc, Mutex, RwLock,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    time::{Duration, Instant},
};
use windshare_relay_core::{identifiers::ShareId, sessions::SessionKey};

const THROUGHPUT_BOUNDS: [u64; 6] = [64_000, 256_000, 1_000_000, 4_000_000, 16_000_000, u64::MAX];

/// Node-local cumulative load summary logged periodically by `wsrelay`.
///
/// Detailed forwarding-write timing and throughput buckets stay in per-node
/// logs; only a lightweight subset is exported to the authority control plane.
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

#[derive(Default)]
struct SessionProgress {
    bytes: u64,
    active: Duration,
    last_end: Option<Instant>,
    ended: bool,
}

/// Per-session accumulator shared only between one session's own directions and
/// lifecycle retirement, avoiding contention on the node-wide session table.
struct SessionHandle {
    key: SessionKey,
    ended: AtomicBool,
    progress: Mutex<SessionProgress>,
}

impl SessionHandle {
    fn new(key: SessionKey) -> Self {
        Self {
            key,
            ended: AtomicBool::new(false),
            progress: Mutex::new(SessionProgress::default()),
        }
    }

    fn is_ended(&self) -> bool {
        self.ended.load(Ordering::Acquire)
    }

    fn record(&self, bytes: u64, start: Instant, end: Instant) -> Option<Duration> {
        let mut progress = self.progress.lock().expect("session metrics");
        if progress.ended {
            return None;
        }
        // Concurrent directions belong to one session's active-time union.
        let interval_start = progress
            .last_end
            .map_or(start, |last_end| start.max(last_end));
        let active = end.saturating_duration_since(interval_start);
        progress.active += active;
        progress.bytes = progress.bytes.saturating_add(bytes);
        progress.last_end = Some(progress.last_end.map_or(end, |last_end| last_end.max(end)));
        drop(progress);
        Some(active)
    }

    fn finish(&self) -> (u64, Duration) {
        let mut progress = self.progress.lock().expect("session metrics");
        progress.ended = true;
        self.ended.store(true, Ordering::Release);
        let totals = (progress.bytes, progress.active);
        drop(progress);
        totals
    }
}

#[derive(Default)]
struct SessionsState {
    active: BTreeMap<ShareId, Vec<Arc<SessionHandle>>>,
    active_sessions: u64,
    completed_sessions: u64,
    throughput_sessions: [u64; 6],
}

#[derive(Default)]
struct Inner {
    sessions: RwLock<SessionsState>,
    forwarded_bytes: AtomicU64,
    wire_bytes: AtomicU64,
    forwarding_nanoseconds: AtomicU64,
    admission_refusals: AtomicU64,
    queue_refusals: AtomicU64,
}

#[derive(Clone, Default)]
pub struct Metrics(Arc<Inner>);

/// Connection-local forwarding recorder that caches active session handles so
/// steady-state writes never acquire the node-wide session table lock.
pub struct ForwardingRecorder {
    metrics: Metrics,
    sessions: Vec<Arc<SessionHandle>>,
}

impl ForwardingRecorder {
    pub fn forwarded(&mut self, key: SessionKey, bytes: u64, start: Instant, end: Instant) {
        self.metrics
            .0
            .forwarded_bytes
            .fetch_add(bytes, Ordering::Relaxed);
        if let Some(index) = self.sessions.iter().position(|entry| entry.key == key) {
            if self
                .metrics
                .record_handle(&self.sessions[index], bytes, start, end)
            {
                if index != 0 {
                    self.sessions.swap(0, index);
                }
                return;
            }
            self.sessions.swap_remove(index);
        }
        self.sessions.retain(|entry| !entry.is_ended());
        if let Some(handle) = self.metrics.lookup(key)
            && self.metrics.record_handle(&handle, bytes, start, end)
        {
            self.sessions.push(handle);
            let last = self.sessions.len() - 1;
            self.sessions.swap(0, last);
        }
    }
}

impl Metrics {
    pub fn recorder(&self) -> ForwardingRecorder {
        ForwardingRecorder {
            metrics: self.clone(),
            sessions: Vec::new(),
        }
    }

    pub fn begin(&self, key: SessionKey) {
        let mut state = self.0.sessions.write().expect("node metrics");
        let sessions = state.active.entry(key.share()).or_default();
        if sessions.iter().any(|entry| entry.key == key) {
            return;
        }
        sessions.push(Arc::new(SessionHandle::new(key)));
        state.active_sessions += 1;
    }

    pub fn end(&self, key: SessionKey) {
        let mut state = self.0.sessions.write().expect("node metrics");
        let Some(sessions) = state.active.get_mut(&key.share()) else {
            return;
        };
        let Some(index) = sessions.iter().position(|entry| entry.key == key) else {
            return;
        };
        let session = sessions.swap_remove(index);
        if sessions.is_empty() {
            state.active.remove(&key.share());
        }
        let (bytes, active) = session.finish();
        state.active_sessions -= 1;
        state.completed_sessions += 1;
        if bytes > 0 {
            let rate = (u128::from(bytes) * 1_000_000_000 / active.as_nanos().max(1))
                .min(u128::from(u64::MAX));
            let bucket = THROUGHPUT_BOUNDS
                .iter()
                .position(|bound| rate <= u128::from(*bound))
                .expect("unbounded bucket");
            state.throughput_sessions[bucket] += 1;
        }
    }

    #[cfg(test)]
    pub fn forwarded(&self, key: SessionKey, bytes: u64, start: Instant, end: Instant) {
        self.0.forwarded_bytes.fetch_add(bytes, Ordering::Relaxed);
        if let Some(handle) = self.lookup(key) {
            let _ = self.record_handle(&handle, bytes, start, end);
        }
    }

    fn lookup(&self, key: SessionKey) -> Option<Arc<SessionHandle>> {
        self.0
            .sessions
            .read()
            .expect("node metrics")
            .active
            .get(&key.share())
            .and_then(|sessions| sessions.iter().find(|entry| entry.key == key))
            .cloned()
    }

    fn record_handle(
        &self,
        handle: &SessionHandle,
        bytes: u64,
        start: Instant,
        end: Instant,
    ) -> bool {
        let Some(active) = handle.record(bytes, start, end) else {
            return false;
        };
        self.0.forwarding_nanoseconds.fetch_add(
            u64::try_from(active.as_nanos()).unwrap_or(u64::MAX),
            Ordering::Relaxed,
        );
        true
    }

    pub fn wire(&self, bytes: usize) {
        self.0.wire_bytes.fetch_add(bytes as u64, Ordering::Relaxed);
    }

    pub fn admission_refused(&self) {
        self.0.admission_refusals.fetch_add(1, Ordering::Relaxed);
    }

    pub fn queue_refused(&self) {
        self.0.queue_refusals.fetch_add(1, Ordering::Relaxed);
    }

    pub fn snapshot(&self) -> Snapshot {
        let state = self.0.sessions.read().expect("node metrics");
        let snapshot = Snapshot {
            active_sessions: state.active_sessions,
            forwarded_bytes: self.0.forwarded_bytes.load(Ordering::Relaxed),
            wire_bytes: self.0.wire_bytes.load(Ordering::Relaxed),
            forwarding_nanoseconds: self.0.forwarding_nanoseconds.load(Ordering::Relaxed),
            completed_sessions: state.completed_sessions,
            admission_refusals: self.0.admission_refusals.load(Ordering::Relaxed),
            queue_refusals: self.0.queue_refusals.load(Ordering::Relaxed),
            throughput_bytes_per_second_bounds: THROUGHPUT_BOUNDS,
            throughput_sessions: state.throughput_sessions,
        };
        drop(state);
        snapshot
    }
}

#[cfg(test)]
#[path = "metrics_tests.rs"]
mod tests;
