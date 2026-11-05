use std::time::Duration;

pub const FLIGHT_DURATION: Duration = Duration::from_millis(1350);
pub const LANDING_DURATION: Duration = Duration::from_millis(720);
pub const STORY_DURATION: Duration = FLIGHT_DURATION.saturating_add(LANDING_DURATION);
const RESULT_FADE: Duration = Duration::from_millis(180);
const LINK_ACTIVITY_CYCLE: Duration = Duration::from_millis(1500);
const STILL_ACTIVITY_PHASE: f32 = 0.5;

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
    pub result_visibility: f32,
    pub activity_phase: f32,
    pub phase: PresentationPhase,
}

impl ShareFrame {
    pub fn sample(elapsed: Duration, ready_at: Option<Duration>, still: bool) -> Self {
        // Readiness controls usable content. The presentation clock only controls choreography.
        let story = if still {
            STORY_DURATION
        } else {
            elapsed.min(STORY_DURATION)
        };
        // Waiting outlives the finite introduction. Its activity follows the
        // operation clock so the indicator keeps moving until a result arrives.
        let activity_phase = if still {
            STILL_ACTIVITY_PHASE
        } else {
            let cycle = LINK_ACTIVITY_CYCLE.as_secs_f32();
            elapsed
                .saturating_sub(FLIGHT_DURATION)
                .as_secs_f32()
                .rem_euclid(cycle)
                / cycle
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
            result_visibility,
            activity_phase,
            phase,
        }
    }

    pub const fn needs_animation(self) -> bool {
        !matches!(self.phase, PresentationPhase::Complete)
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
