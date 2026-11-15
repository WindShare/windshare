use super::{super::SpiritMood, behavior::Pose};
use gpui::{Point, point};

#[derive(Clone, Copy)]
pub(super) struct Presentation {
    pub mood: SpiritMood,
    pub center: Point<f32>,
    pub width: f32,
    pub reduced_motion: bool,
}

impl Presentation {
    pub fn sprite_pose(self, pose: Pose, interaction_scale: f32) -> (Point<f32>, f32, f32) {
        (
            self.center + point(pose.x * interaction_scale, pose.y * interaction_scale),
            self.width,
            pose.tilt,
        )
    }

    pub fn smile(self, greeting: bool) -> f32 {
        if self.mood == SpiritMood::Happy {
            1.
        } else {
            f32::from(u8::from(greeting))
        }
    }
}
