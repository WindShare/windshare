use std::time::{Duration, Instant};

const MIN_FLIGHT_MS: u64 = 1650;
pub const MIN_FLIGHT_DURATION: Duration = Duration::from_millis(MIN_FLIGHT_MS);
pub const FLIGHT_LAUNCH_SECONDS: f32 = MIN_FLIGHT_MS as f32 / 1000.;
const ARRIVAL_DURATION: Duration = Duration::from_millis(350);

#[derive(Clone, Copy, Debug)]
pub struct ShareArrival {
    started_at: Instant,
    flight_elapsed: Duration,
}

#[derive(Clone, Copy)]
pub struct ArrivalFrame {
    pub source_seconds: f32,
    pub flight_seconds: f32,
    pub progress: f32,
}

impl ShareArrival {
    pub fn new(flight_started_at: Instant, reveal_at: Instant) -> Self {
        let flight_elapsed = reveal_at.saturating_duration_since(flight_started_at);
        Self {
            started_at: reveal_at,
            flight_elapsed,
        }
    }

    pub fn duration(self) -> Duration {
        ARRIVAL_DURATION
    }

    pub fn sample(self, now: Instant) -> ArrivalFrame {
        let elapsed = now.saturating_duration_since(self.started_at);
        ArrivalFrame {
            // Continue the source trajectory while blending out, preserving its velocity.
            source_seconds: (self.flight_elapsed + elapsed).as_secs_f32(),
            flight_seconds: self.flight_elapsed.as_secs_f32(),
            progress: (elapsed.as_secs_f32() / ARRIVAL_DURATION.as_secs_f32()).clamp(0., 1.),
        }
    }
}

pub fn segment(value: f32, start: f32, end: f32) -> f32 {
    ((value - start) / (end - start)).clamp(0., 1.)
}

pub fn smooth(t: f32) -> f32 {
    // Zero endpoint velocity and acceleration keep an interrupted flight from snapping.
    t * t * t * (t * (t * 6. - 15.) + 10.)
}

pub fn ease_out(t: f32) -> f32 {
    1. - (1. - t).powi(4)
}

pub fn lerp(a: f32, b: f32, t: f32) -> f32 {
    a + (b - a) * t
}
