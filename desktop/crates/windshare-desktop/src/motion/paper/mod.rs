//! The original paper-and-ribbon entrance, sampled from the share run's clock.
//! Artwork stays in paint coordinates; only the result owns layout.
mod wind;

use super::{SpiritOrigin, SpiritPlacement, lerp, whirl::easing::smoothstep};
use crate::artwork::Artwork;
use gpui::{Bounds, Div, Pixels, Point, Radians, canvas, div, point, prelude::*};
use std::f32::consts::PI;

pub const FLIGHT: f32 = 1.35;
pub const LANDING: f32 = 0.72;
pub const DURATION: f32 = FLIGHT + LANDING;
pub const PROMPT_DEPARTURE: f32 = FLIGHT * 0.25;
const LANDED_SPIRIT_SIZE: f32 = 52.;
const SPIRIT_EXPANSION: f32 = 64.;
const PAPER_COUNT: u16 = 7;
const PAPER_ASSETS: [Artwork; 3] = [Artwork::Paper, Artwork::Photo, Artwork::Folder];

pub fn card_presence(seconds: f32) -> f32 {
    smoothstep(0.24, 1., landing(seconds))
}

fn landing(seconds: f32) -> f32 {
    ((seconds - FLIGHT) / LANDING).clamp(0., 1.)
}

fn destination(width: f32, height: f32) -> Point<f32> {
    point(width * 0.68, height * 0.24)
}

pub fn scenery(origin: SpiritOrigin, seconds: f32) -> Div {
    let progress = (seconds / FLIGHT).clamp(0., 1.);
    // One continuous clock keeps the ribbon moving across the landing boundary.
    let head = lerp(0.18, 1.75, smoothstep(0., DURATION, seconds));
    let strength = if seconds >= FLIGHT {
        0.8 * (1. - smoothstep(0., 0.8, landing(seconds)))
    } else {
        smoothstep(0., 0.14, progress) * lerp(1., 0.8, ((progress - 0.9) / 0.1).clamp(0., 1.))
    };
    let presence = 1. - smoothstep(0.12, 0.72, landing(seconds));
    div()
        .absolute()
        .inset_0()
        .size_full()
        .child(wind::wind_field(head, strength))
        .children((0..PAPER_COUNT).filter_map(|index| paper(index, progress)))
        .child(
            div().absolute().inset_0().opacity(presence).child(
                canvas(
                    |_, _, _| (),
                    move |bounds, (), window, _| {
                        let pose = spirit_pose(origin.resolve(bounds), bounds, seconds);
                        Artwork::Spirit
                            .blend_to(Artwork::Happy, pose.center, pose.width, pose.smile)
                            .rotate(Radians(pose.tilt.to_radians()))
                            .paint(Point::default(), window);
                    },
                )
                .absolute()
                .inset_0(),
            ),
        )
}

fn spirit_pose(launch: SpiritPlacement, bounds: Bounds<Pixels>, seconds: f32) -> SpiritPlacement {
    let origin = point(f32::from(bounds.left()), f32::from(bounds.top()));
    let width = f32::from(bounds.size.width);
    let height = f32::from(bounds.size.height);
    let end = destination(width, height);
    let progress = smoothstep(0., DURATION, seconds);
    let greeting = 1. - smoothstep(0., PROMPT_DEPARTURE, seconds);
    SpiritPlacement {
        center: origin
            + cubic(
                [
                    launch.center - origin,
                    point(width * 0.14, height * 0.63),
                    end + point(-110., 36.),
                    end,
                ],
                progress,
            ),
        width: SPIRIT_EXPANSION.mul_add(
            (PI * progress).sin().powi(2),
            lerp(launch.width, LANDED_SPIRIT_SIZE, progress),
        ),
        tilt: launch.tilt * greeting,
        smile: lerp(
            launch.smile * greeting,
            1.,
            smoothstep(0.02, 0.42, landing(seconds)),
        ),
    }
}

#[allow(
    clippy::suboptimal_flops,
    reason = "Control-point formulas keep per-paper spatial and timing offsets visible."
)]
fn paper(index: u16, progress: f32) -> Option<Div> {
    let seed = f32::from(index);
    let approach = smoothstep(seed * 0.028, 0.88 + seed * 0.018, progress);
    // Fade on the approach, before all files reach the same endpoint.
    let opacity = smoothstep(0.01, 0.13, progress) * (1. - smoothstep(0.90, 1., approach));
    (opacity > 0.).then(|| {
        div().absolute().inset_0().opacity(opacity).child(
            canvas(
                |_, _, _| (),
                move |bounds, (), window, _| {
                    let width = f32::from(bounds.size.width);
                    let height = f32::from(bounds.size.height);
                    let flank = if index.is_multiple_of(2) { -0.12 } else { 1.10 };
                    let center = cubic(
                        [
                            point(width * (0.39 + seed * 0.035), height * 0.57),
                            point(width * flank, height * (0.62 - seed * 0.045)),
                            point(
                                width * (0.12 + seed * 0.115),
                                height * (-0.25 + seed * 0.035),
                            ),
                            destination(width, height) + point(-46., 28.),
                        ],
                        approach,
                    );
                    PAPER_ASSETS[usize::from(index) % PAPER_ASSETS.len()]
                        .sprite(center, lerp(54. + seed * 3., 27., approach))
                        .paint(bounds.origin, window);
                },
            )
            .absolute()
            .inset_0(),
        )
    })
}

#[allow(
    clippy::suboptimal_flops,
    reason = "Keep Bernstein weights visible instead of nesting fused coordinate operations."
)]
fn cubic(points: [Point<f32>; 4], t: f32) -> Point<f32> {
    let [start, first, second, end] = points;
    let remaining = 1. - t;
    let start_weight = remaining.powi(3);
    let first_weight = 3. * remaining * remaining * t;
    let second_weight = 3. * remaining * t * t;
    let end_weight = t.powi(3);
    point(
        start_weight * start.x
            + first_weight * first.x
            + second_weight * second.x
            + end_weight * end.x,
        start_weight * start.y
            + first_weight * first.y
            + second_weight * second.y
            + end_weight * end.y,
    )
}
