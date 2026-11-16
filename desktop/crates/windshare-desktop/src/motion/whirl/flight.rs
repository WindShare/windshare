//! The spirit's trip from where the user last saw it to the eye of the QR slot.

use super::easing::{ease_in_out_cubic, smoothstep, swell};
use crate::motion::lerp;
use gpui::{Point, point};
use std::f32::consts::TAU;

/// Seconds the spirit gathers itself before leaving.
pub const WINDUP: f32 = 0.14;
/// Seconds in the air.
pub const TRAVEL: f32 = 0.72;
pub const ARRIVAL: f32 = WINDUP + TRAVEL;
/// Seconds the slowest mote trails a moving eye, stretching the whirl into a comet.
pub const TRAIL: f32 = 0.2;
/// From here on every mote is back around the eye, so the slot can own the whole whirl.
pub const HOME: f32 = ARRIVAL + TRAIL;

const RECOIL_SPAN: f32 = 0.24;
const RECOIL_DISTANCE: f32 = 10.;
const RECOIL_LEAN: f32 = 7.;
const ARC_RATIO: f32 = 0.34;
const ARC_LIMITS: (f32, f32) = (26., 88.);
const SHRINK_SPAN: (f32, f32) = (0.08, 1.);
const BANK_PER_SPEED: f32 = 0.026;
const MAX_BANK: f32 = 15.;
const VELOCITY_STEP: f32 = 1. / 240.;
const HOVER_PERIOD: f32 = 3.4;
const HOVER_AMPLITUDE: f32 = 2.5;
const HOVER_ONSET: f32 = 0.6;
const EMERGENCE: f32 = 0.18;
/// The resting spirit's width as a share of the slot's side: small enough to
/// leave the whirl room, large enough to keep its face readable.
const SLOT_SPIRIT: f32 = 0.23;

/// Width of a spirit resting in the eye of a slot.
pub fn resting_width(slot_side: f32) -> f32 {
    slot_side * SLOT_SPIRIT
}

#[derive(Clone, Copy)]
pub struct Flight {
    pub from: Point<f32>,
    pub to: Point<f32>,
    pub from_width: f32,
    pub to_width: f32,
}

#[derive(Clone, Copy)]
pub struct Pose {
    pub center: Point<f32>,
    pub width: f32,
    /// Degrees, clockwise on screen.
    pub tilt: f32,
}

impl Flight {
    /// Eased share of the trip completed.
    pub fn progress(seconds: f32) -> f32 {
        ease_in_out_cubic((seconds - WINDUP) / TRAVEL)
    }

    /// Where the whirl is centered. Motes sample this in their own past, so it
    /// must stay defined before the flight starts.
    pub fn eye(&self, seconds: f32) -> Point<f32> {
        let chord = self.to - self.from;
        let length = chord.x.hypot(chord.y);
        if length < f32::EPSILON {
            return self.to;
        }
        let heading = chord / length;
        // Bulge upward for sideways trips; a mirrored normal would dip a
        // leftward flight into the content below.
        let normal = if heading.x >= 0. {
            point(heading.y, -heading.x)
        } else {
            point(-heading.y, heading.x)
        };
        let lift = (ARC_RATIO * length).clamp(ARC_LIMITS.0, ARC_LIMITS.1);
        let crest = (self.from + self.to) / 2. + normal * lift;
        let t = Self::progress(seconds);
        let recoil = RECOIL_DISTANCE * swell(seconds.max(0.) / RECOIL_SPAN);
        self.from * (1. - t).powi(2) + crest * (2. * t * (1. - t)) + self.to * t.powi(2)
            - heading * recoil
    }

    /// Share of the way from the launch figure to the resting one. The spirit
    /// and the band of its whirl follow this together, so they shrink as one body.
    pub fn settling(seconds: f32) -> f32 {
        smoothstep(SHRINK_SPAN.0, SHRINK_SPAN.1, Self::progress(seconds))
    }

    pub fn width(&self, seconds: f32) -> f32 {
        lerp(self.from_width, self.to_width, Self::settling(seconds))
    }

    pub fn pose(&self, seconds: f32) -> Pose {
        let velocity = (self.eye(seconds + VELOCITY_STEP) - self.eye(seconds - VELOCITY_STEP))
            / (2. * VELOCITY_STEP);
        let heading = (self.to.x - self.from.x).signum();
        let lean = RECOIL_LEAN * heading * swell(seconds.max(0.) / RECOIL_SPAN);
        Pose {
            center: self.eye(seconds) + point(0., hover(seconds - ARRIVAL)),
            width: self.width(seconds),
            tilt: (velocity.x * BANK_PER_SPEED).clamp(-MAX_BANK, MAX_BANK) - lean,
        }
    }
}

/// Vertical bob of a spirit resting in the eye, `seconds` after it arrived.
pub fn hover(seconds: f32) -> f32 {
    let seconds = seconds.max(0.);
    HOVER_AMPLITUDE * smoothstep(0., HOVER_ONSET, seconds) * (TAU * seconds / HOVER_PERIOD).sin()
}

/// Presence of a spirit that has no previous position to continue from.
pub fn emergence(seconds: f32) -> f32 {
    smoothstep(0., EMERGENCE, seconds)
}
