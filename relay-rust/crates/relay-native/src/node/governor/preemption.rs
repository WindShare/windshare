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
// After Restore confirms contention by lifting public egress for one sample,
// owner TCP congestion control and local queues may need up to two samples
// (500 ms) to climb back above the release threshold.
const RESTORE_RECOVERY_SAMPLES: u8 = 2;
const RAMP_DIVISOR: u64 = 4;
const UPLINK_MARGIN_DIVISOR: u64 = 20;
const PROBE_GAIN_DIVISOR: u64 = 5;
const RESTORE_DROP_DIVISOR: u64 = 10;
const RELEASE_NUMERATOR: u128 = 2;
const RELEASE_DENOMINATOR: u128 = 3;

#[derive(Default)]
pub(super) struct Egress {
    owner: AtomicU64,
    public: AtomicU64,
}

impl Egress {
    pub fn record(&self, owner: bool, bytes: u64) {
        if owner { &self.owner } else { &self.public }.fetch_add(bytes, Ordering::Relaxed);
    }

    fn drain(&self) -> (u64, u64) {
        (
            self.owner.swap(0, Ordering::Relaxed),
            self.public.swap(0, Ordering::Relaxed),
        )
    }
}

#[derive(Clone, Copy)]
struct Sample {
    owner: u64,
    public: u64,
    now: Instant,
}

#[derive(Clone, Copy)]
struct ProbeState {
    previous_owner: u64,
    previous_cap: u64,
    previous_public: u64,
    protected_rate: Option<u64>,
}

#[derive(Clone, Copy, Default)]
enum HintedPhase {
    #[default]
    Idle,
    Ramp,
    Tracking,
}

impl HintedPhase {
    const fn name(self) -> &'static str {
        match self {
            Self::Idle => "idle",
            Self::Ramp => "ramp",
            Self::Tracking => "tracking",
        }
    }

    fn step(&mut self, cap: u64, uplink: u64, owner: u64) -> u64 {
        let floor = SIGNAL_FLOOR.min(cap);
        if owner < ACTIVE_RATE {
            *self = Self::Idle;
            cap
        } else if matches!(self, Self::Idle) {
            *self = Self::Ramp;
            floor.max(cap.min(uplink) / RAMP_DIVISOR)
        } else {
            *self = Self::Tracking;
            cap.min(
                uplink
                    .saturating_sub(owner)
                    .saturating_sub(uplink / UPLINK_MARGIN_DIVISOR)
                    .max(floor),
            )
        }
    }
}

#[derive(Clone, Copy, Default)]
enum AdaptivePhase {
    #[default]
    Idle,
    Cooldown {
        until: Instant,
    },
    Probe(ProbeState),
    Tracking {
        protected_rate: u64,
    },
    Recover {
        protected_rate: u64,
        hold_until: Instant,
        remaining_samples: u8,
    },
    Hold {
        until: Instant,
        protected_rate: u64,
    },
    Restore {
        owner: u64,
        retained: u64,
    },
}

impl AdaptivePhase {
    const fn name(self) -> &'static str {
        match self {
            Self::Idle => "idle",
            Self::Cooldown { .. } => "cooldown",
            Self::Probe(_) => "probe",
            Self::Tracking { .. } => "tracking",
            Self::Recover { .. } => "recover",
            Self::Hold { .. } => "hold",
            Self::Restore { .. } => "restore",
        }
    }

    fn step(&mut self, cap: u64, current: u64, sample: Sample) -> u64 {
        if sample.owner < ACTIVE_RATE {
            *self = Self::Idle;
            return cap;
        }
        match *self {
            Self::Cooldown { until } if sample.now < until => cap,
            Self::Idle | Self::Cooldown { .. } => self.probe_or_cooldown(cap, sample),
            Self::Probe(probe) => self.evaluate_probe(cap, current, probe, sample),
            Self::Tracking { protected_rate } => {
                self.evaluate_tracking(cap, current, protected_rate, sample)
            }
            Self::Recover {
                protected_rate,
                hold_until,
                remaining_samples,
            } => self.evaluate_recover(
                cap,
                current,
                protected_rate,
                hold_until,
                remaining_samples,
                sample,
            ),
            Self::Hold {
                until,
                protected_rate,
            } => self.evaluate_hold(cap, current, until, protected_rate, sample),
            Self::Restore { owner, retained } => {
                self.evaluate_restore(cap, owner, retained, sample)
            }
        }
    }

    fn probe_or_cooldown(&mut self, cap: u64, sample: Sample) -> u64 {
        let floor = SIGNAL_FLOOR.min(cap);
        if sample.public > floor && cap > floor {
            self.begin_probe(cap, floor, None, sample)
        } else {
            *self = Self::Cooldown {
                until: sample.now + PROBE_INTERVAL,
            };
            cap
        }
    }

    fn begin_probe(
        &mut self,
        current: u64,
        floor: u64,
        protected_rate: Option<u64>,
        sample: Sample,
    ) -> u64 {
        *self = Self::Probe(ProbeState {
            previous_owner: sample.owner,
            previous_cap: current,
            previous_public: sample.public,
            protected_rate,
        });
        (sample.public.min(current) / 2).max(floor)
    }

    fn evaluate_probe(&mut self, cap: u64, current: u64, probe: ProbeState, sample: Sample) -> u64 {
        if probe
            .protected_rate
            .is_some_and(|rate| below_release_threshold(sample.owner, rate))
        {
            *self = Self::Idle;
            return cap;
        }
        // Keep a reduction only when the owner used a material part of the
        // reclaimed bandwidth. A remote slow writer fails this test.
        let gain = probe.previous_public.saturating_sub(current) / PROBE_GAIN_DIVISOR;
        if sample.owner
            > probe
                .previous_owner
                .saturating_add(gain.max(ACTIVE_RATE / 2))
        {
            *self = Self::Tracking {
                protected_rate: sample.owner,
            };
            current
        } else if let Some(protected_rate) = probe.protected_rate {
            *self = Self::Hold {
                until: sample.now + PROBE_INTERVAL,
                protected_rate,
            };
            probe.previous_cap
        } else {
            *self = Self::Cooldown {
                until: sample.now + PROBE_INTERVAL,
            };
            cap
        }
    }

    fn evaluate_tracking(
        &mut self,
        cap: u64,
        current: u64,
        protected_rate: u64,
        sample: Sample,
    ) -> u64 {
        if below_release_threshold(sample.owner, protected_rate) {
            *self = Self::Idle;
            return cap;
        }
        let floor = SIGNAL_FLOOR.min(cap);
        if sample.public > floor && current > floor {
            self.begin_probe(current, floor, Some(protected_rate), sample)
        } else {
            *self = Self::Hold {
                until: sample.now + PROBE_INTERVAL,
                protected_rate,
            };
            current
        }
    }

    fn evaluate_recover(
        &mut self,
        cap: u64,
        current: u64,
        protected_rate: u64,
        hold_until: Instant,
        remaining_samples: u8,
        sample: Sample,
    ) -> u64 {
        if sample.now >= hold_until {
            if below_release_threshold(sample.owner, protected_rate) {
                *self = Self::Idle;
            } else {
                *self = Self::Restore {
                    owner: sample.owner,
                    retained: current,
                };
            }
            return cap;
        }
        if !below_release_threshold(sample.owner, protected_rate) || remaining_samples <= 1 {
            *self = Self::Hold {
                until: hold_until,
                protected_rate,
            };
        } else {
            *self = Self::Recover {
                protected_rate,
                hold_until,
                remaining_samples: remaining_samples - 1,
            };
        }
        current
    }

    fn evaluate_hold(
        &mut self,
        cap: u64,
        current: u64,
        until: Instant,
        protected_rate: u64,
        sample: Sample,
    ) -> u64 {
        if below_release_threshold(sample.owner, protected_rate) {
            *self = Self::Idle;
            cap
        } else if sample.now < until {
            current
        } else {
            *self = Self::Restore {
                owner: sample.owner,
                retained: current,
            };
            cap
        }
    }

    fn evaluate_restore(&mut self, cap: u64, protected: u64, retained: u64, sample: Sample) -> u64 {
        let drop = (protected / RESTORE_DROP_DIVISOR).max(ACTIVE_RATE / 8);
        if sample.owner < protected.saturating_sub(drop) {
            *self = Self::Recover {
                protected_rate: protected,
                hold_until: sample.now + PROBE_INTERVAL,
                remaining_samples: RESTORE_RECOVERY_SAMPLES,
            };
            retained
        } else {
            *self = Self::Cooldown {
                until: sample.now + PROBE_INTERVAL,
            };
            cap
        }
    }
}

#[derive(Default)]
enum Mode {
    #[default]
    Disabled,
    Hinted {
        cap: u64,
        uplink: u64,
        current: u64,
        phase: HintedPhase,
    },
    Adaptive {
        cap: u64,
        current: u64,
        phase: AdaptivePhase,
    },
}

impl Mode {
    const fn current(&self) -> Option<u64> {
        match *self {
            Self::Disabled => None,
            Self::Hinted { current, .. } | Self::Adaptive { current, .. } => Some(current),
        }
    }

    const fn phase_name(&self) -> &'static str {
        match *self {
            Self::Disabled => "disabled",
            Self::Hinted { phase, .. } => phase.name(),
            Self::Adaptive { phase, .. } => phase.name(),
        }
    }

    const fn same_config(&self, other: &Self) -> bool {
        match (self, other) {
            (Self::Disabled, Self::Disabled) => true,
            (
                Self::Hinted {
                    cap: a_cap,
                    uplink: a_uplink,
                    ..
                },
                Self::Hinted {
                    cap: b_cap,
                    uplink: b_uplink,
                    ..
                },
            ) => *a_cap == *b_cap && *a_uplink == *b_uplink,
            (Self::Adaptive { cap: a_cap, .. }, Self::Adaptive { cap: b_cap, .. }) => {
                *a_cap == *b_cap
            }
            _ => false,
        }
    }
}

#[derive(Default)]
pub(super) struct Preemption {
    mode: Mode,
    measured: Option<Instant>,
}

impl Preemption {
    pub const fn configure(&mut self, cap: Option<u64>, uplink: Option<u64>) -> bool {
        let next = match (cap, uplink) {
            (None, _) => Mode::Disabled,
            (Some(cap), Some(uplink)) => Mode::Hinted {
                cap,
                uplink,
                current: cap,
                phase: HintedPhase::Idle,
            },
            (Some(cap), None) => Mode::Adaptive {
                cap,
                current: cap,
                phase: AdaptivePhase::Idle,
            },
        };
        if self.mode.same_config(&next) {
            return false;
        }
        self.mode = next;
        self.measured = None;
        true
    }

    pub fn sample(&mut self, wire: &Egress, now: Instant) -> Option<u64> {
        if matches!(self.mode, Mode::Disabled) {
            return None;
        }
        let Some(last) = self.measured else {
            // Discard bytes recorded before this sampling window opened
            // (startup, bypass traffic, or prior settings) so the first window
            // cannot inflate into a phantom spike.
            let _ = wire.drain();
            self.measured = Some(now);
            return self.mode.current();
        };
        let elapsed = now.duration_since(last);
        if elapsed < SAMPLE {
            return self.mode.current();
        }
        self.measured = Some(now);
        let (owner_bytes, public_bytes) = wire.drain();
        let sample = Sample {
            owner: rate_per_second(owner_bytes, elapsed),
            public: rate_per_second(public_bytes, elapsed),
            now,
        };
        let (previous, next) = match &mut self.mode {
            Mode::Disabled => return None,
            Mode::Hinted {
                cap,
                uplink,
                current,
                phase,
            } => {
                let previous = *current;
                let next = phase.step(*cap, *uplink, sample.owner);
                *current = next;
                (previous, next)
            }
            Mode::Adaptive {
                cap,
                current,
                phase,
            } => {
                let previous = *current;
                let next = phase.step(*cap, previous, sample);
                *current = next;
                (previous, next)
            }
        };
        if previous != next {
            tracing::debug!(
                event = "owner_preemption",
                phase = self.mode.phase_name(),
                owner_bytes_per_second = sample.owner,
                public_bytes_per_second = sample.public,
                previous,
                public_limit = next
            );
        }
        Some(next)
    }
}

fn rate_per_second(bytes: u64, elapsed: Duration) -> u64 {
    u64::try_from(u128::from(bytes) * 1_000_000_000 / elapsed.as_nanos()).unwrap_or(u64::MAX)
}

fn below_release_threshold(owner: u64, protected_rate: u64) -> bool {
    u128::from(owner) < u128::from(protected_rate) * RELEASE_NUMERATOR / RELEASE_DENOMINATOR
}

#[cfg(test)]
#[path = "preemption_tests.rs"]
mod tests;
