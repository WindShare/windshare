use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};

#[derive(Default)]
pub(super) struct Metrics {
    pub(super) healthy: AtomicBool,
    pub(super) received: AtomicU64,
    pub(super) responded: AtomicU64,
    pub(super) invalid: AtomicU64,
    pub(super) limited: AtomicU64,
    pub(super) write_errors: AtomicU64,
    pub(super) read_errors: AtomicU64,
}

pub(super) struct ListenerStatus {
    pub(super) id: usize,
    pub(super) metrics: Metrics,
}

impl ListenerStatus {
    pub(super) fn new(id: usize) -> Self {
        Self {
            id,
            metrics: Metrics::default(),
        }
    }

    pub(super) fn healthy(&self) -> bool {
        self.metrics.healthy.load(Ordering::Acquire)
    }
}

pub(super) struct Serving<'a>(pub(super) &'a ListenerStatus);

impl<'a> Serving<'a> {
    pub(super) fn begin(status: &'a ListenerStatus) -> Self {
        status.metrics.healthy.store(true, Ordering::Release);
        Self(status)
    }
}

impl Drop for Serving<'_> {
    fn drop(&mut self) {
        self.0.metrics.healthy.store(false, Ordering::Release);
        tracing::info!(
            event = "stun_listener_stopped",
            listener_id = self.0.id,
            received = self.0.metrics.received.load(Ordering::Relaxed),
            responded = self.0.metrics.responded.load(Ordering::Relaxed),
            invalid = self.0.metrics.invalid.load(Ordering::Relaxed),
            limited = self.0.metrics.limited.load(Ordering::Relaxed),
            write_errors = self.0.metrics.write_errors.load(Ordering::Relaxed),
            read_errors = self.0.metrics.read_errors.load(Ordering::Relaxed),
        );
    }
}
