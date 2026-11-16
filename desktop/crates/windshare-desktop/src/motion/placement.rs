use super::SpiritScene;
use gpui::{Bounds, Pixels, Point, point};
use std::{cell::Cell, rc::Rc};

/// The pose actually drawn, with its center in window coordinates. Entrances
/// inherit this snapshot so ambient motion and pointer feedback cannot jump.
#[derive(Clone, Copy, Debug)]
pub struct SpiritPlacement {
    pub center: Point<f32>,
    pub width: f32,
    pub tilt: f32,
    pub smile: f32,
}

/// A flight always has a spirit, even when activation bypasses the choose page.
/// Resolve a fresh scene against its own painted bounds, not window-size guesses.
#[derive(Clone, Copy)]
pub enum SpiritOrigin {
    Inherited(SpiritPlacement),
    Scene { scale: f32 },
}

impl SpiritOrigin {
    pub fn resolve(self, bounds: Bounds<Pixels>) -> SpiritPlacement {
        match self {
            Self::Inherited(placement) => placement,
            Self::Scene { scale } => {
                let center = bounds.center();
                let scene_corner = point(
                    f32::from(center.x) - SpiritScene::width(scale) / 2.,
                    f32::from(center.y) - SpiritScene::height(scale) / 2.,
                );
                SpiritPlacement {
                    center: scene_corner + SpiritScene::body_center(scale),
                    width: SpiritScene::body_size(scale),
                    tilt: 0.,
                    smile: 0.,
                }
            }
        }
    }
}

pub type SpiritAnchor = Rc<Cell<Option<SpiritPlacement>>>;
