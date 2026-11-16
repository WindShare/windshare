use gpui::Point;
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

pub type SpiritAnchor = Rc<Cell<Option<SpiritPlacement>>>;
