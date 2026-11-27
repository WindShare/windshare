use std::{
    sync::atomic::{AtomicU64, Ordering},
    time::Duration,
};
use tokio::time::Instant;

// A full-size transfer creates sustained writes on this scale. Heartbeats and
// occasional receiver acknowledgements should never take a public rate lease.
const SAMPLE: Duration = Duration::from_millis(250);
const ACTIVE_RATE: u64 = 32 * 1024;
const SIGNAL_FLOOR: u64 = 8 * 1024;
const PROBE_INTERVAL: Duration = Duration::from_secs(2);

#[derive(Default)]
pub(super) struct Egress {
    owner: AtomicU64,
    public: AtomicU64,
}
impl Egress {
    pub fn record(&self, owner: bool, bytes: u64) {
        if owner { &self.owner } else { &self.public }.fetch_add(bytes, Ordering::Relaxed);
    }
}

#[derive(Default)]
enum Phase {
    #[default]
    Idle,
    Ramp,
    Tracking,
    Probe {
        previous_owner: u64,
        previous_cap: u64,
        previous_public: u64,
    },
    Hold {
        until: Instant,
    },
    Restore {
        owner: u64,
        retained: u64,
    },
}

#[derive(Default)]
pub(super) struct Preemption {
    cap: Option<u64>,
    uplink: Option<u64>,
    current: Option<u64>,
    measured: Option<Instant>,
    phase: Phase,
    protected_rate: u64,
}

impl Preemption {
    pub fn configure(&mut self, cap: Option<u64>, uplink: Option<u64>) -> bool {
        if self.cap == cap && self.uplink == uplink {
            return false;
        }
        *self = Self {
            cap,
            uplink,
            current: cap,
            ..Self::default()
        };
        true
    }

    pub fn sample(&mut self, wire: &Egress, now: Instant) -> Option<u64> {
        let cap = self.cap?;
        let last = *self.measured.get_or_insert(now);
        let elapsed = now.duration_since(last);
        if elapsed < SAMPLE {
            return self.current;
        }
        self.measured = Some(now);
        let rate = |counter: &AtomicU64| {
            u64::try_from(
                u128::from(counter.swap(0, Ordering::Relaxed)) * 1_000_000_000 / elapsed.as_nanos(),
            )
            .unwrap_or(u64::MAX)
        };
        let owner = rate(&wire.owner);
        let public = rate(&wire.public);
        let current = self.current.unwrap_or(cap);
        let floor = SIGNAL_FLOOR.min(cap);
        let released =
            current < cap && self.protected_rate > 0 && owner < self.protected_rate * 2 / 3;
        let next = if owner < ACTIVE_RATE || released {
            self.protected_rate = 0;
            self.phase = Phase::Idle;
            cap
        } else if let Some(uplink) = self.uplink {
            if matches!(self.phase, Phase::Idle) {
                self.phase = Phase::Ramp;
                floor.max(cap / 4)
            } else {
                self.phase = Phase::Tracking;
                cap.min(
                    uplink
                        .saturating_sub(owner)
                        .saturating_sub(uplink / 20)
                        .max(floor),
                )
            }
        } else {
            match self.phase {
                Phase::Probe {
                    previous_owner,
                    previous_cap,
                    previous_public,
                } => {
                    // Keep a reduction only when the owner used a material part
                    // of the reclaimed bandwidth. A remote slow writer fails this test.
                    let gain = previous_public.saturating_sub(current) / 5;
                    if owner > previous_owner.saturating_add(gain.max(ACTIVE_RATE / 2)) {
                        self.protected_rate = owner;
                        self.phase = Phase::Tracking;
                        current
                    } else {
                        self.phase = Phase::Hold {
                            until: now + PROBE_INTERVAL,
                        };
                        previous_cap
                    }
                }
                Phase::Restore {
                    owner: protected,
                    retained,
                } => {
                    self.phase = Phase::Hold {
                        until: now + PROBE_INTERVAL,
                    };
                    if owner < protected.saturating_sub((protected / 10).max(ACTIVE_RATE / 8)) {
                        retained
                    } else {
                        cap
                    }
                }
                Phase::Hold { until } if now < until => current,
                Phase::Hold { .. } if current < cap => {
                    self.phase = Phase::Restore {
                        owner,
                        retained: current,
                    };
                    cap
                }
                _ if public > floor && current > floor => {
                    self.phase = Phase::Probe {
                        previous_owner: owner,
                        previous_cap: current,
                        previous_public: public,
                    };
                    (public.min(current) / 2).max(floor)
                }
                _ => {
                    self.phase = Phase::Hold {
                        until: now + PROBE_INTERVAL,
                    };
                    current
                }
            }
        };
        if current != next {
            tracing::debug!(
                event = "owner_preemption",
                owner_bytes_per_second = owner,
                public_bytes_per_second = public,
                previous = current,
                public_limit = next
            );
        }
        self.current = Some(next);
        self.current
    }
}

#[cfg(test)]
#[path = "preemption_tests.rs"]
mod tests;
