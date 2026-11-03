use std::time::Duration;

pub const FLIGHT_DURATION: Duration = Duration::from_millis(1350);
pub const LANDING_DURATION: Duration = Duration::from_millis(720);
pub const STORY_DURATION: Duration = FLIGHT_DURATION.saturating_add(LANDING_DURATION);
const RESULT_FADE: Duration = Duration::from_millis(180);
const BREATH_CYCLE_SECONDS: f32 = 3.4;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PresentationPhase {
    Flight,
    Landing,
    WaitingForLink,
    RevealingLink,
    Complete,
}

#[derive(Clone, Copy)]
pub struct ShareFrame {
    pub story: Duration,
    pub breath: f32,
    pub result_visibility: f32,
    pub phase: PresentationPhase,
}

impl ShareFrame {
    pub fn sample(elapsed: Duration, ready_at: Option<Duration>, still: bool) -> Self {
        // Readiness controls usable content. The presentation clock only controls choreography.
        let story = if still {
            if ready_at.is_some() {
                STORY_DURATION
            } else {
                Duration::ZERO
            }
        } else {
            elapsed.min(STORY_DURATION)
        };
        let result_visibility = ready_at.map_or(0., |ready| {
            if still {
                1.
            } else {
                smooth(
                    (elapsed.saturating_sub(ready).as_secs_f32() / RESULT_FADE.as_secs_f32())
                        .min(1.),
                )
            }
        });
        let breath = if still {
            0.
        } else {
            (elapsed.saturating_sub(STORY_DURATION).as_secs_f32() / BREATH_CYCLE_SECONDS
                * std::f32::consts::TAU)
                .sin()
        };
        let phase = if story < FLIGHT_DURATION {
            PresentationPhase::Flight
        } else if story < STORY_DURATION {
            PresentationPhase::Landing
        } else if ready_at.is_none() {
            PresentationPhase::WaitingForLink
        } else if result_visibility < 1. {
            PresentationPhase::RevealingLink
        } else {
            PresentationPhase::Complete
        };
        Self {
            story,
            breath,
            result_visibility,
            phase,
        }
    }
}

pub fn segment(value: f32, start: f32, end: f32) -> f32 {
    ((value - start) / (end - start)).clamp(0., 1.)
}

#[allow(
    clippy::suboptimal_flops,
    reason = "Keep the standard smoothstep polynomial recognizable."
)]
pub fn smooth(t: f32) -> f32 {
    t * t * (3. - 2. * t)
}

pub fn ease_out(t: f32) -> f32 {
    1. - (1. - t).powi(4)
}

pub fn lerp(a: f32, b: f32, t: f32) -> f32 {
    (b - a).mul_add(t, a)
}
