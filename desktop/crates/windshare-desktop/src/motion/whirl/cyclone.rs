//! The whirl: motes streaming along spiral arms into the eye the spirit occupies.
//!
//! Every mote is a pure function of time, so a whirl can change painters, skip
//! frames or resume after a hidden window without drifting or popping.

use super::easing::{ease_out_cubic, smoothstep};
use crate::motion::lerp;
use gpui::{Point, point};
use std::f32::consts::TAU;

pub const MOTES: u16 = 300;
const ARMS: u16 = 3;
/// Radians per second of the rigid arm pattern, clockwise on screen.
const PATTERN_SPEED: f32 = 0.85;
/// Winding between rim and eye. Falling motes overtake the pattern, so arms trail.
const SWIRL: f32 = 4.;
const ARM_SPREAD: f32 = 0.7;
const ARM_TAPER: f32 = 0.45;
const LIFE: (f32, f32) = (1.6, 2.8);
const SIDE: (f32, f32) = (1.9, 4.4);
/// Above one, motes linger near the eye and the core reads denser than the rim.
const INFALL: f32 = 1.4;
const RADIAL_SCATTER: f32 = 0.14;
const FADE_IN: f32 = 0.16;
const FADE_OUT: f32 = 0.74;
const SPROUT: f32 = 0.26;
const SPROUT_STAGGER: f32 = 0.1;
const SPROUT_REACH: f32 = 0.3;
const EYE_RATIO: f32 = 0.5;
/// Width of the band around a launching spirit, as a share of its width.
const LAUNCH_BAND: f32 = 0.34;
/// Motes scale with the spirit they surround so both read as one body in flight.
const LAUNCH_GRAIN: f32 = 1.7;
/// The resting rim as a share of the slot's side, clear of its border.
const SLOT_RIM: f32 = 0.44;
const SPIN: (f32, f32) = (1., 3.4);
const PALE_SHARE: f32 = 0.3;
const DRAG: (f32, f32) = (0.15, 1.);
const DRAG_NEAR_EYE: f32 = 0.4;
/// Share of its radius a mote gives up while the eye swallows the whirl.
const COLLAPSE_PULL: f32 = 0.6;

#[derive(Clone, Copy, PartialEq, Eq)]
pub enum Shade {
    Deep,
    Pale,
}

#[derive(Clone, Copy)]
pub struct Mote {
    /// Offset from the whirl's eye.
    pub offset: Point<f32>,
    pub side: f32,
    /// Radians, clockwise on screen.
    pub turn: f32,
    pub shade: Shade,
    /// Share of the trail this mote keeps behind a moving eye.
    pub drag: f32,
}

/// The band the motes occupy around the spirit.
#[derive(Clone, Copy)]
pub struct Whirl {
    pub eye: f32,
    pub rim: f32,
    /// Multiplier on every mote's side.
    pub grain: f32,
}

impl Whirl {
    pub fn launching(spirit_width: f32) -> Self {
        Self {
            eye: spirit_width * EYE_RATIO,
            rim: spirit_width * (EYE_RATIO + LAUNCH_BAND),
            grain: LAUNCH_GRAIN,
        }
    }

    pub fn resting(spirit_width: f32, slot_side: f32) -> Self {
        Self {
            eye: spirit_width * EYE_RATIO,
            rim: slot_side * SLOT_RIM,
            grain: 1.,
        }
    }

    pub fn blend(self, other: Self, t: f32) -> Self {
        Self {
            eye: lerp(self.eye, other.eye, t),
            rim: lerp(self.rim, other.rim, t),
            grain: lerp(self.grain, other.grain, t),
        }
    }

    /// `seconds` counts from the whirl's first frame; `collapse` runs from zero
    /// to one while the eye swallows it.
    pub fn mote(self, index: u16, seconds: f32, collapse: f32) -> Mote {
        let seconds = seconds.max(0.);
        let life = lerp(LIFE.0, LIFE.1, noise(index, 0));
        // Zero at the rim, one at the eye; the wrap is hidden by a zero-size envelope.
        let fall = (noise(index, 1) + seconds / life).fract();
        let lateral = noise(index, 2) + noise(index, 3) - 1.;
        let arm = TAU * f32::from(index % ARMS) / f32::from(ARMS);
        let angle = (lateral * ARM_SPREAD).mul_add(
            ARM_TAPER.mul_add(-fall, 1.),
            PATTERN_SPEED.mul_add(seconds, SWIRL.mul_add(fall, arm)),
        );
        let reach =
            ((1. - fall).powf(INFALL) * RADIAL_SCATTER.mul_add(noise(index, 4) - 0.5, 1.)).min(1.);
        let sprout = smoothstep(
            0.,
            SPROUT,
            SPROUT_STAGGER.mul_add(-noise(index, 5), seconds),
        );
        let radius = (self.rim - self.eye).mul_add(
            reach * lerp(SPROUT_REACH, 1., ease_out_cubic(sprout)),
            self.eye,
        ) * COLLAPSE_PULL.mul_add(-collapse, 1.);
        let envelope = smoothstep(0., FADE_IN, fall) * (1. - smoothstep(FADE_OUT, 1., fall));
        let direction = if index.is_multiple_of(2) { 1. } else { -1. };
        Mote {
            offset: point(angle.cos(), angle.sin()) * radius,
            side: self.grain
                * lerp(SIDE.0, SIDE.1, noise(index, 6))
                * envelope
                * sprout
                * (1. - collapse),
            turn: TAU.mul_add(
                noise(index, 7),
                direction * lerp(SPIN.0, SPIN.1, noise(index, 8)) * seconds,
            ),
            shade: if noise(index, 9) < PALE_SHARE {
                Shade::Pale
            } else {
                Shade::Deep
            },
            drag: lerp(DRAG.0, DRAG.1, noise(index, 10)) * lerp(DRAG_NEAR_EYE, 1., reach),
        }
    }
}

/// Stable per-mote traits in `0..1` without stored state.
fn noise(index: u16, channel: u32) -> f32 {
    let mut bits = u32::from(index)
        .wrapping_mul(0x9E37_79B9)
        .wrapping_add(channel.wrapping_mul(0x85EB_CA6B));
    bits ^= bits >> 16;
    bits = bits.wrapping_mul(0x7FEB_352D);
    bits ^= bits >> 15;
    bits = bits.wrapping_mul(0x846C_A68B);
    bits ^= bits >> 16;
    let [low, high, ..] = bits.to_le_bytes();
    f32::from(u16::from_le_bytes([low, high])) / 65_536.
}
