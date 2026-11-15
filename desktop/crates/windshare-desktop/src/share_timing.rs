use std::time::Duration;

pub const FLIGHT_DURATION: Duration = Duration::from_millis(1350);
pub const LANDING_DURATION: Duration = Duration::from_millis(720);
pub const STORY_DURATION: Duration = FLIGHT_DURATION.saturating_add(LANDING_DURATION);
const RESULT_REVEAL_DURATION: Duration = Duration::from_millis(440);
const LINK_ACTIVITY_CYCLE: Duration = Duration::from_millis(1800);
const STILL_ACTIVITY_PHASE: f32 = 0.5;
const CONTROLS_REVEAL_END: f32 = 0.75;
const QR_REVEAL_START: f32 = 0.08;
const DETAILS_REVEAL_START: f32 = 0.35;
const WAITING_EXIT_END: f32 = 0.55;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PresentationPhase {
    Flight,
    Landing,
    WaitingForLink,
    RevealingLink,
    Complete,
}

/// Presentation never gates copying or scanning a link that is already usable.
#[derive(Clone, Copy)]
pub enum LinkPresentation {
    Waiting { activity_phase: f32 },
    Revealing { progress: f32, activity_phase: f32 },
    Ready,
}

impl LinkPresentation {
    pub const fn settled(ready: bool) -> Self {
        if ready {
            Self::Ready
        } else {
            Self::Waiting {
                activity_phase: STILL_ACTIVITY_PHASE,
            }
        }
    }

    fn sample(elapsed: Duration, ready_at: Option<Duration>, still: bool) -> Self {
        if still {
            return Self::settled(ready_at.is_some());
        }
        let cycle = LINK_ACTIVITY_CYCLE.as_secs_f32();
        let activity_phase = elapsed
            .saturating_sub(FLIGHT_DURATION)
            .as_secs_f32()
            .rem_euclid(cycle)
            / cycle;
        let Some(ready) = ready_at else {
            return Self::Waiting { activity_phase };
        };
        let reveal_elapsed = elapsed.saturating_sub(ready);
        if reveal_elapsed >= RESULT_REVEAL_DURATION {
            Self::Ready
        } else {
            Self::Revealing {
                progress: reveal_elapsed.as_secs_f32() / RESULT_REVEAL_DURATION.as_secs_f32(),
                activity_phase,
            }
        }
    }

    pub const fn activity_phase(self) -> f32 {
        match self {
            Self::Waiting { activity_phase } | Self::Revealing { activity_phase, .. } => {
                activity_phase
            }
            Self::Ready => STILL_ACTIVITY_PHASE,
        }
    }

    pub fn content_visibility(self) -> f32 {
        smooth(segment(self.progress(), 0., CONTROLS_REVEAL_END))
    }

    pub fn details_visibility(self) -> f32 {
        smooth(segment(self.progress(), DETAILS_REVEAL_START, 1.))
    }

    pub fn qr_visibility(self) -> f32 {
        smooth(segment(self.progress(), QR_REVEAL_START, 1.))
    }

    pub fn waiting_visibility(self) -> f32 {
        1. - smooth(segment(self.progress(), 0., WAITING_EXIT_END))
    }

    const fn progress(self) -> f32 {
        match self {
            Self::Waiting { .. } => 0.,
            Self::Revealing { progress, .. } => progress,
            Self::Ready => 1.,
        }
    }
}

#[derive(Clone, Copy)]
pub struct ShareFrame {
    pub story: Duration,
    pub link: LinkPresentation,
    pub phase: PresentationPhase,
}

impl ShareFrame {
    pub fn sample(elapsed: Duration, ready_at: Option<Duration>, still: bool) -> Self {
        // The finite introduction and indefinite waiting share one operation clock.
        let story = if still {
            STORY_DURATION
        } else {
            elapsed.min(STORY_DURATION)
        };
        let link = LinkPresentation::sample(elapsed, ready_at, still);
        let phase = if story < FLIGHT_DURATION {
            PresentationPhase::Flight
        } else if story < STORY_DURATION {
            PresentationPhase::Landing
        } else {
            match link {
                LinkPresentation::Waiting { .. } => PresentationPhase::WaitingForLink,
                LinkPresentation::Revealing { .. } => PresentationPhase::RevealingLink,
                LinkPresentation::Ready => PresentationPhase::Complete,
            }
        };
        Self { story, link, phase }
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

pub fn lerp(a: f32, b: f32, t: f32) -> f32 {
    (b - a).mul_add(t, a)
}
