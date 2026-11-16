//! The share reveal: the spirit rides a whirl of motes into the QR slot, and
//! the eye of that whirl opens into the code.
//!
//! Every figure is a pure function of a clock, so painters hold no state and a
//! flight can hand its whirl to a slot without a seam between the two.

pub mod bloom;
mod cyclone;
pub mod easing;
pub mod flight;
mod mesh;

use crate::{artwork::Artwork, motion::lerp};
use cyclone::{MOTES, Mote, Shade, Whirl};
use flight::{ARRIVAL, Flight, HOME, TRAIL};
use gpui::{
    BorderStyle, Bounds, Hsla, Pixels, Point, Radians, Rgba, Window, point, px, quad, rgb, size,
    transparent_black,
};
use mesh::Squares;
use std::time::Instant;
use windshare_ui::{QrCodeImage, Theme, Tone};

/// Earliest second of a run at which the eye may open. By then the whirl has
/// stood long enough to read as a beat of its own rather than a stumble
/// between flight and bloom, and a link that was already waiting settles two
/// seconds after the run began.
pub const HOLD: f32 = 1.4;

/// Clock of a whirl shown where nothing may move, late enough that every mote has sprouted.
const STILL_CLOCK: f32 = 6.;
/// Modules leave the eye in the wind's green. Fixed across themes because the
/// plate beneath them is always white.
const SEED_INK: u32 = 0x2f_65_52;
const VISIBLE_SPIRIT: f32 = 0.5;

/// How a whirl came to stand in a slot, and the clock it has run on since.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Wind {
    /// Flown in around the spirit. The flight paints it until every mote is home.
    Carried { since: Instant },
    /// Risen in place around a spirit that has no previous position.
    Rooted { since: Instant },
}

impl Wind {
    /// The whirl as its slot shows it, or `None` while a flight still carries it there.
    pub fn standing(self, now: Instant) -> Option<Standing> {
        match self {
            Self::Carried { since } => {
                let clock = seconds_between(since, now);
                (clock >= HOME).then(|| Standing {
                    clock,
                    hover: flight::hover(clock - ARRIVAL),
                    presence: 1.,
                })
            }
            Self::Rooted { since } => {
                let clock = seconds_between(since, now);
                Some(Standing {
                    clock,
                    hover: flight::hover(clock),
                    presence: flight::emergence(clock),
                })
            }
        }
    }
}

pub fn seconds_between(earlier: Instant, later: Instant) -> f32 {
    later.saturating_duration_since(earlier).as_secs_f32()
}

/// A standing whirl at one instant.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Standing {
    /// Seconds on the whirl's own clock.
    pub clock: f32,
    /// Vertical offset of the resting spirit.
    pub hover: f32,
    /// Share of its resting size the spirit has reached.
    pub presence: f32,
}

impl Standing {
    /// Reduced motion keeps the whirl as a picture.
    pub const STILL: Self = Self {
        clock: STILL_CLOCK,
        hover: 0.,
        presence: 1.,
    };
}

#[derive(Clone, Copy)]
pub struct Tones {
    deep: Hsla,
    pale: Hsla,
}

impl Tones {
    pub fn of(theme: Theme) -> Self {
        Self {
            deep: theme.accent_text(),
            pale: Tone::Mint.accent(theme),
        }
    }
}

/// The eye of a standing whirl opening into a code.
pub struct Opening<'a> {
    pub code: &'a QrCodeImage,
    /// Corner radius of the settled plate.
    pub corner: f32,
    pub progress: f32,
}

/// Where a flight starts: the spirit as the user last saw it, in window coordinates.
#[derive(Clone, Copy)]
pub struct Launch {
    pub center: Point<f32>,
    pub width: f32,
}

/// The whirl in the air: motes trail the spirit from `launch` into `slot`.
pub fn paint_flight(
    window: &mut Window,
    tones: Tones,
    launch: Launch,
    slot: Bounds<Pixels>,
    seconds: f32,
) {
    let (center, side) = seat(slot);
    let flight = Flight {
        from: launch.center,
        to: center,
        from_width: launch.width,
        to_width: flight::resting_width(side),
    };
    let whirl = Whirl::launching(flight.from_width).blend(
        Whirl::resting(flight.to_width, side),
        Flight::settling(seconds),
    );
    paint_motes(window, tones, whirl, seconds, 0., |mote| {
        flight.eye(mote.drag.mul_add(-TRAIL, seconds))
    });
    let pose = flight.pose(seconds);
    paint_spirit(window, pose.center, pose.width, pose.tilt, 0.);
}

/// The whirl standing in `slot` around the resting spirit.
pub fn paint_standing(window: &mut Window, tones: Tones, slot: Bounds<Pixels>, standing: Standing) {
    let (center, side) = seat(slot);
    let spirit = flight::resting_width(side);
    let whirl = Whirl::resting(spirit, side);
    paint_motes(window, tones, whirl, standing.clock, 0., |_| center);
    paint_spirit(
        window,
        center + point(0., standing.hover),
        spirit * standing.presence,
        0.,
        0.,
    );
}

/// The eye releasing the code: the plate opens, modules spiral out to their
/// cells, the whirl falls into the eye and the spirit leaves.
pub fn paint_bloom(
    window: &mut Window,
    tones: Tones,
    slot: Bounds<Pixels>,
    standing: Standing,
    opening: &Opening,
) {
    let (center, side) = seat(slot);
    let spirit = flight::resting_width(side);
    let whirl = Whirl::resting(spirit, side);
    let progress = opening.progress;

    let plate = bloom::plate(progress, whirl.eye, side / 2., opening.corner);
    window.paint_quad(quad(
        Bounds::centered_at(
            point(px(center.x), px(center.y)),
            size(px(plate.half * 2.), px(plate.half * 2.)),
        ),
        px(plate.corner),
        rgb(QrCodeImage::GROUND),
        0.,
        transparent_black(),
        BorderStyle::default(),
    ));

    let grid = opening.code.grid(slot, window.scale_factor());
    let cell = f32::from(grid.cell);
    let rests: Vec<Point<f32>> = opening
        .code
        .dark_modules()
        .map(|(x, y)| {
            let module = grid.module_center(x, y);
            point(f32::from(module.x), f32::from(module.y)) - center
        })
        .collect();
    let reach = rests
        .iter()
        .map(|rest| rest.x.hypot(rest.y))
        .fold(0., f32::max);
    let mut petals = Squares::with_capacity(rests.len());
    for rest in rests {
        let petal = bloom::petal(rest, reach, progress);
        petals.push(center + petal.offset, cell * petal.scale, petal.turn);
    }
    petals.paint(window, ink(progress));

    let collapse = bloom::collapse(progress);
    paint_motes(window, tones, whirl, standing.clock, collapse, |_| center);
    let leave = bloom::leave(progress);
    paint_spirit(
        window,
        center + point(0., standing.hover),
        spirit * standing.presence * leave.scale,
        leave.spin,
        1.,
    );
}

fn seat(slot: Bounds<Pixels>) -> (Point<f32>, f32) {
    let center = slot.center();
    (
        point(f32::from(center.x), f32::from(center.y)),
        f32::from(slot.size.width.min(slot.size.height)),
    )
}

/// `eye` places each mote's center, so a moving whirl can let its motes trail.
fn paint_motes(
    window: &mut Window,
    tones: Tones,
    whirl: Whirl,
    clock: f32,
    collapse: f32,
    eye: impl Fn(&Mote) -> Point<f32>,
) {
    let mut deep = Squares::with_capacity(usize::from(MOTES));
    let mut pale = Squares::with_capacity(usize::from(MOTES));
    for index in 0..MOTES {
        let mote = whirl.mote(index, clock, collapse);
        let batch = match mote.shade {
            Shade::Deep => &mut deep,
            Shade::Pale => &mut pale,
        };
        batch.push(eye(&mote) + mote.offset, mote.side, mote.turn);
    }
    deep.paint(window, tones.deep);
    pale.paint(window, tones.pale);
}

/// `tilt` is in degrees, clockwise on screen; `smile` blends toward the happy face.
fn paint_spirit(window: &mut Window, center: Point<f32>, width: f32, tilt: f32, smile: f32) {
    if width < VISIBLE_SPIRIT {
        return;
    }
    Artwork::Spirit
        .blend_to(Artwork::Happy, center, width, smile)
        .rotate(Radians(tilt.to_radians()))
        .paint(Point::default(), window);
}

fn ink(progress: f32) -> Rgba {
    let (seed, settled) = (rgb(SEED_INK), rgb(QrCodeImage::INK));
    let mixed = bloom::ink(progress);
    Rgba {
        r: lerp(seed.r, settled.r, mixed),
        g: lerp(seed.g, settled.g, mixed),
        b: lerp(seed.b, settled.b, mixed),
        a: 1.,
    }
}

#[cfg(test)]
#[path = "whirl_tests.rs"]
mod tests;
