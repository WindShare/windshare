use super::{super::SpiritMood, behavior::Pose};
use crate::share_timing::{lerp, segment, smooth};
use gpui::{Point, point};

const INTERACTION_HANDOFF_END: f32 = 0.18;

#[derive(Clone, Copy)]
pub enum SpiritState {
    Floating(SpiritMood),
    Choreographed { progress: f32, smile: f32 },
}

impl SpiritState {
    fn interaction_visibility(self) -> f32 {
        match self {
            Self::Floating(_) => 1.,
            Self::Choreographed { progress, .. } => {
                1. - smooth(segment(progress, 0., INTERACTION_HANDOFF_END))
            }
        }
    }
}

#[derive(Clone, Copy)]
pub(super) struct Presentation {
    pub state: SpiritState,
    pub center: Point<f32>,
    pub width: f32,
    pub reduced_motion: bool,
}

impl Presentation {
    pub const fn hit_center(self) -> Option<Point<f32>> {
        match self.state {
            SpiritState::Floating(_) => Some(self.center),
            SpiritState::Choreographed { .. } => None,
        }
    }

    pub fn sprite_pose(self, pose: Pose, interaction_scale: f32) -> (Point<f32>, f32, f32) {
        // The authored flight takes over gradually from the current interaction
        // pose, preserving the first frame even when the pointer has just left.
        let visibility = self.state.interaction_visibility();
        let residual = visibility * interaction_scale;
        (
            self.center + point(pose.x * residual, pose.y * residual),
            self.width,
            pose.tilt * visibility,
        )
    }

    pub fn smile(self, greeting: bool) -> f32 {
        let greeting = f32::from(u8::from(greeting));
        match self.state {
            SpiritState::Choreographed { smile, .. } => {
                lerp(smile, greeting, self.state.interaction_visibility())
            }
            SpiritState::Floating(SpiritMood::Happy) => 1.,
            SpiritState::Floating(_) => greeting,
        }
    }
}
