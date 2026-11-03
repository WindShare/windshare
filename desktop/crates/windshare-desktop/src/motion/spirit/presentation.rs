use super::super::SpiritMood;
use gpui::Point;

#[derive(Clone, Copy)]
pub enum SpiritState {
    Floating(SpiritMood),
    Choreographed { smile: f32 },
    Settled { anchor: Point<f32> },
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
            SpiritState::Settled { anchor } => Some(anchor),
        }
    }

    pub fn smile(self, greeting: bool) -> f32 {
        match self.state {
            SpiritState::Choreographed { smile } => smile,
            SpiritState::Settled { .. } | SpiritState::Floating(SpiritMood::Happy) => 1.,
            SpiritState::Floating(_) => f32::from(u8::from(greeting)),
        }
    }
}
