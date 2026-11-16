//! The bloom: the eye releases the code, each module spiralling out to its cell.

use super::easing::{ease_in_cubic, ease_out_cubic, ease_out_quint, smoothstep, swell};
use crate::motion::lerp;
use gpui::{Point, point};
use std::f32::consts::TAU;

/// Seconds from the first released module to the settled code.
pub const BLOOM: f32 = 0.6;
/// Seconds of the ring that confirms the settled code.
pub const HALO: f32 = 0.7;

/// Share of the bloom over which modules leave; the rest is each module's trip.
const STAGGER: f32 = 0.52;
/// Share of the release order decided by distance; the arms decide the rest.
const RADIAL_ORDER: f32 = 0.7;
const ARMS: f32 = 3.;
/// Turns the releasing arms wind between eye and rim, matching the whirl's trailing figure.
const ARM_WINDING: f32 = 0.55;
/// Radians a module near the eye still has to turn around it when it leaves.
const TWIST: f32 = 1.9;
/// Ranks over which the twist dies out. Modules beyond the frame's inscribed
/// circle would leave the frame if they turned, so the corners fly straight.
const TWIRL: (f32, f32) = (0.3, 0.74);
const GROW: (f32, f32) = (0.05, 0.7);
const PLATE_SPAN: f32 = 0.72;
const PLATE_SEED: f32 = 0.6;
const SQUARING: (f32, f32) = (0.3, 1.);
const POP: f32 = 0.16;
const POP_SPAN: f32 = 0.2;
const LEAVE_SPAN: (f32, f32) = (0.08, 0.36);
const LEAVE_SPIN: f32 = 150.;
const COLLAPSE_SPAN: f32 = 0.34;
const INK_SPAN: (f32, f32) = (0.35, 0.95);
const HALO_SPREAD: f32 = 7.;
const HALO_OPACITY: f32 = 0.5;

#[derive(Clone, Copy)]
pub struct Petal {
    /// Offset from the eye.
    pub offset: Point<f32>,
    /// Share of the module's settled side.
    pub scale: f32,
    /// Radians, clockwise on screen.
    pub turn: f32,
}

/// `rest` is the module's settled offset from the eye and `reach` the farthest
/// module's distance, which orders the release from the eye outward.
pub fn petal(rest: Point<f32>, reach: f32, progress: f32) -> Petal {
    let distance = rest.x.hypot(rest.y);
    let bearing = rest.y.atan2(rest.x);
    let rank = if reach > 0. {
        (distance / reach).min(1.)
    } else {
        0.
    };
    // A smooth three-lobed order: a sawtooth sweep would tear neighbours apart in time.
    let sweep = (TAU * ARM_WINDING).mul_add(rank, ARMS * bearing);
    let arm = 0.5_f32.mul_add(-sweep.cos(), 0.5);
    let release = STAGGER * lerp(arm, rank, RADIAL_ORDER);
    let local = ((progress - release) / (1. - STAGGER)).clamp(0., 1.);
    if local >= 1. {
        // The settled frame must equal the static code exactly, not within rounding.
        return Petal {
            offset: rest,
            scale: 1.,
            turn: 0.,
        };
    }
    // Distance settles before bearing, so the last thing seen is the whirl
    // winding down rather than the code zooming.
    let twist = TWIST * (1. - smoothstep(TWIRL.0, TWIRL.1, rank));
    let unwound = -twist * (1. - ease_out_cubic(local));
    let angle = bearing + unwound;
    Petal {
        offset: point(angle.cos(), angle.sin()) * (distance * ease_out_quint(local)),
        scale: smoothstep(GROW.0, GROW.1, local),
        turn: unwound,
    }
}

/// The scanner's white ground, opening as a disc and squaring off into the frame.
#[derive(Clone, Copy)]
pub struct Plate {
    pub half: f32,
    pub corner: f32,
}

pub fn plate(progress: f32, eye: f32, half: f32, corner: f32) -> Plate {
    let grown = ease_out_cubic(progress / PLATE_SPAN);
    let extent = lerp(eye * PLATE_SEED, half, grown);
    Plate {
        half: extent,
        corner: lerp(extent, corner, smoothstep(SQUARING.0, SQUARING.1, grown)),
    }
}

/// The spirit's exit from the eye: a pleased pop, then a spin into nothing.
#[derive(Clone, Copy)]
pub struct Leave {
    pub scale: f32,
    /// Degrees, clockwise on screen.
    pub spin: f32,
}

pub fn leave(progress: f32) -> Leave {
    let gone = ease_in_cubic((progress - LEAVE_SPAN.0) / (LEAVE_SPAN.1 - LEAVE_SPAN.0));
    Leave {
        scale: POP.mul_add(swell(progress / POP_SPAN), 1.) * (1. - gone),
        spin: LEAVE_SPIN * gone,
    }
}

/// How far the eye has swallowed the whirl.
pub fn collapse(progress: f32) -> f32 {
    smoothstep(0., COLLAPSE_SPAN, progress)
}

/// Zero while modules still wear the wind's color, one once they are scanner black.
pub fn ink(progress: f32) -> f32 {
    smoothstep(INK_SPAN.0, INK_SPAN.1, progress)
}

/// Spread in pixels and opacity of the confirming ring, `seconds` after settling.
pub fn halo(seconds: f32) -> Option<(f32, f32)> {
    let x = seconds / HALO;
    (0. ..1.).contains(&x).then(|| {
        (
            HALO_SPREAD * ease_out_cubic(x),
            HALO_OPACITY * (1. - x).powi(2),
        )
    })
}
