use crate::{
    layout::{FlowLayout, task_width},
    share_timing::{
        ArrivalFrame, FLIGHT_LAUNCH_SECONDS, ShareArrival, ease_out, lerp, segment, smooth,
    },
};
use gpui::{prelude::*, *};
use std::{
    f32::consts::{PI, TAU},
    time::{Duration, Instant},
};
use windshare_ui::*;

const READY_WIDTH: f32 = 620.;
const READY_HEIGHT: f32 = 352.;
pub const SHARE_LINK_HEIGHT: f32 = 78.;
const SHARE_LINK_TOP: f32 = 116.;
const LANDED_SPIRIT_SIZE: f32 = 92.;
const CRUISING_SPIRIT_SIZE: f32 = 144.;
const PAPER_COUNT: usize = 7;
const WIND_SEGMENTS: usize = 48;
const CRUISE_CYCLE_SECONDS: f32 = 3.2;
const CRUISE_BLEND_SECONDS: f32 = 0.5;
const PREPARATION_FADE_SECONDS: f32 = 0.18;
const PAPER_ASSETS: [&str; 3] = ["brand/paper.svg", "brand/photo.svg", "brand/folder.svg"];

#[derive(Clone, Copy)]
pub struct ShareGeometry {
    pub width: f32,
    pub height: f32,
    pub content_width: f32,
    pub content_left: f32,
    pub content_top: f32,
    initial_spirit_size: f32,
}

impl ShareGeometry {
    pub fn new(window: &Window, layout: FlowLayout) -> Self {
        let width = task_width(window, layout);
        let content_width = width.min(READY_WIDTH);
        Self {
            width,
            height: layout.stage_height(),
            content_width,
            content_left: (width - content_width) / 2.,
            content_top: (layout.stage_height() - READY_HEIGHT) / 2.,
            initial_spirit_size: 156. * layout.scene_scale(),
        }
    }
    fn destination(self) -> Point<f32> {
        point(
            self.content_left + self.content_width - 88.,
            self.content_top + 48.,
        )
    }
    fn flight_end(self) -> Point<f32> {
        let destination = self.destination();
        point(destination.x - 46., destination.y + 28.)
    }
    pub fn link_top(self) -> f32 {
        self.content_top + SHARE_LINK_TOP
    }
    fn link_anchor(self) -> Point<f32> {
        // The paper destination follows the actual link row when its layout changes.
        point(
            self.content_left + 30.,
            self.link_top() + SHARE_LINK_HEIGHT / 2.,
        )
    }
}

#[derive(Clone, Copy)]
enum Phase {
    Flight(f32),
    Arrival(ArrivalFrame),
    Settled,
}

#[derive(Clone, Copy)]
pub struct ShareScene {
    pub geometry: ShareGeometry,
    phase: Phase,
}

impl ShareScene {
    pub fn flight(geometry: ShareGeometry, started_at: Instant) -> Self {
        Self {
            geometry,
            phase: Phase::Flight(started_at.elapsed().as_secs_f32()),
        }
    }
    pub fn arrival(geometry: ShareGeometry, arrival: ShareArrival) -> Self {
        Self {
            geometry,
            phase: Phase::Arrival(arrival.sample(Instant::now())),
        }
    }
    pub fn settled(geometry: ShareGeometry) -> Self {
        Self {
            geometry,
            phase: Phase::Settled,
        }
    }

    pub fn title_visibility(self) -> f32 {
        self.reveal(0., 0.38)
    }
    pub fn link_expansion(self) -> f32 {
        ease_out(self.reveal(0.04, 0.64))
    }
    pub fn link_visibility(self) -> f32 {
        self.reveal(0.12, 0.48)
    }
    pub fn control_visibility(self) -> f32 {
        // The available copy action is visible from the first ready frame.
        0.5 + 0.5 * self.reveal(0., 0.22)
    }
    pub fn summary_visibility(self) -> f32 {
        self.reveal(0.36, 0.80)
    }
    pub fn footer_visibility(self) -> f32 {
        self.reveal(0.62, 1.)
    }
    pub fn preparation_visibility(self) -> f32 {
        let enter = |seconds| smooth(segment(seconds, 0., PREPARATION_FADE_SECONDS));
        match self.phase {
            Phase::Flight(seconds) => enter(seconds),
            Phase::Arrival(frame) => {
                enter(frame.flight_seconds) * (1. - smooth(segment(frame.progress, 0., 0.26)))
            }
            Phase::Settled => 0.,
        }
    }
    fn reveal(self, start: f32, end: f32) -> f32 {
        match self.phase {
            Phase::Flight(_) => 0.,
            Phase::Arrival(frame) => smooth(segment(frame.progress, start, end)),
            Phase::Settled => 1.,
        }
    }

    pub fn scenery(self, still: bool) -> Div {
        div()
            .absolute()
            .inset_0()
            .size_full()
            .when(!matches!(self.phase, Phase::Settled), |el| {
                el.child(self.wind_field())
                    .children((0..PAPER_COUNT).map(|index| self.paper(index)))
            })
            .child(self.spirit(still))
    }

    fn spirit_pose(self) -> (Point<f32>, f32) {
        let g = self.geometry;
        match self.phase {
            Phase::Flight(seconds) => self.flying_spirit(seconds),
            Phase::Arrival(frame) => {
                let (center, size) = self.flying_spirit(frame.source_seconds);
                let blend = smooth(frame.progress);
                (
                    mix_point(center, g.destination(), blend),
                    lerp(size, LANDED_SPIRIT_SIZE, blend),
                )
            }
            Phase::Settled => (g.destination(), LANDED_SPIRIT_SIZE),
        }
    }

    fn flying_spirit(self, seconds: f32) -> (Point<f32>, f32) {
        let g = self.geometry;
        let (launch, cruise) = flight_clock(seconds);
        let end = g.flight_end();
        let center = cubic(
            [
                point(g.width * 0.5, g.height * 0.235),
                point(g.width * 0.14, g.height * 0.63),
                point(end.x - 110., end.y + 36.),
                end,
            ],
            launch,
        ) + cruise_offset(cruise);
        let size = lerp(g.initial_spirit_size, CRUISING_SPIRIT_SIZE, launch)
            + 64. * (PI * launch).sin().powi(2)
            + 4. * cruise.sin();
        (center, size)
    }

    fn spirit(self, still: bool) -> AnyElement {
        let (center, size) = self.spirit_pose();
        let smile = self.reveal(0.02, 0.42);
        let local_scale = size / 156.;
        let surround = self.reveal(0.55, 1.);
        let mut layer = div()
            .absolute()
            .inset_0()
            .child(
                svg()
                    .path("brand/breeze.svg")
                    .absolute()
                    .left(px(center.x - 170. * local_scale))
                    .top(px(center.y - 82. * local_scale))
                    .w(px(340. * local_scale))
                    .h(px(176. * local_scale))
                    .text_color(Tone::Mint.accent())
                    .opacity(surround),
            )
            .child(
                img("brand/spirit.svg")
                    .absolute()
                    .left(px(center.x - size / 2.))
                    .top(px(center.y - size / 2.))
                    .size(px(size))
                    .opacity(1. - smile),
            )
            .child(
                img("brand/spirit-happy.svg")
                    .absolute()
                    .left(px(center.x - size / 2.))
                    .top(px(center.y - size / 2.))
                    .size(px(size))
                    .opacity(smile),
            );
        for (index, asset) in PAPER_ASSETS.into_iter().enumerate() {
            let (x, y) = [(-82., -8.), (77., -29.), (76., 38.)][index];
            layer = layer.child(
                img(asset)
                    .absolute()
                    .left(px(center.x + x))
                    .top(px(center.y + y))
                    .w(px(23.))
                    .h(px(27.))
                    .opacity(surround),
            );
        }
        if matches!(self.phase, Phase::Settled) && !still {
            layer
                .with_animation(
                    "share-landed-breath",
                    Animation::new(Duration::from_millis(3400)).repeat(),
                    |el, delta| el.top(px((delta * TAU).sin() * 2.)),
                )
                .into_any_element()
        } else {
            layer.into_any_element()
        }
    }

    fn paper(self, index: usize) -> Div {
        let seed = index as f32;
        let (center, size, opacity) = match self.phase {
            Phase::Flight(seconds) => self.flying_paper(index, seconds),
            Phase::Arrival(frame) => {
                let (source, size, opacity) = self.flying_paper(index, frame.source_seconds);
                let p = smooth(segment(frame.progress, seed * 0.01, 0.72 + seed * 0.028));
                let center = mix_point(source, self.geometry.link_anchor(), p)
                    + point(0., -48. * (PI * p).sin().powi(2));
                (
                    center,
                    lerp(size, 10., p),
                    opacity * (1. - smooth(segment(p, 0.70, 1.))),
                )
            }
            Phase::Settled => (self.geometry.link_anchor(), 0., 0.),
        };
        div()
            .absolute()
            .left(px(center.x - size / 2.))
            .top(px(center.y - size * 0.57))
            .w(px(size))
            .h(px(size * 1.14))
            .opacity(opacity)
            .child(img(PAPER_ASSETS[index % PAPER_ASSETS.len()]).size_full())
    }

    fn flying_paper(self, index: usize, seconds: f32) -> (Point<f32>, f32, f32) {
        let g = self.geometry;
        let seed = index as f32;
        let (_, cruise) = flight_clock(seconds);
        let angle = seed / PAPER_COUNT as f32 * TAU;
        let orbit = |phase: f32| point(76. * phase.cos(), 42. * phase.sin());
        let anchor = g.flight_end() + orbit(angle);
        let t = segment(seconds, 0., FLIGHT_LAUNCH_SECONDS);
        let p = smooth(segment(t, seed * 0.028, 0.88 + seed * 0.018));
        let flank = if index.is_multiple_of(2) { -0.12 } else { 1.10 };
        let center = cubic(
            [
                point(g.width * (0.30 + seed * 0.065), g.height * 0.57),
                point(g.width * flank, g.height * (0.62 - seed * 0.045)),
                point(
                    g.width * (0.12 + seed * 0.115),
                    g.height * (-0.25 + seed * 0.035),
                ),
                anchor,
            ],
            p,
        ) + orbit(angle + cruise)
            - orbit(angle)
            + cruise_offset(cruise);
        (
            center,
            lerp(44. + seed * 2., 27., p),
            smooth(segment(seconds, 0.015, 0.14)),
        )
    }

    fn wind_pose(self) -> (f32, f32, f32) {
        let flight = |seconds| {
            let (launch, cruise) = flight_clock(seconds);
            (
                lerp(0.18, 1.15, launch),
                smooth(segment(seconds, 0., 0.16)) * (0.88 + 0.06 * cruise.sin()),
                cruise * 0.6,
            )
        };
        match self.phase {
            Phase::Flight(seconds) => flight(seconds),
            Phase::Arrival(frame) => {
                let (head, strength, ripple) = flight(frame.source_seconds);
                (
                    head + 0.6 * smooth(frame.progress),
                    strength * (1. - smooth(segment(frame.progress, 0., 0.8))),
                    ripple,
                )
            }
            Phase::Settled => (0., 0., 0.),
        }
    }

    fn wind_field(self) -> impl IntoElement {
        canvas(
            |_, _, _| (),
            move |bounds, _, window, _| {
                let (head, strength, ripple) = self.wind_pose();
                for (lane, tone) in [Tone::Mint, Tone::Lavender, Tone::Peach]
                    .into_iter()
                    .enumerate()
                {
                    for (width, color) in [
                        (17., tone.surface().opacity(strength * 0.78)),
                        (1.5, tone.accent().opacity(strength * 0.72)),
                    ] {
                        let mut path = PathBuilder::stroke(px(width));
                        let begin = (head - 1.20).max(0.);
                        for step in 0..=WIND_SEGMENTS {
                            let t = lerp(begin, head, step as f32 / WIND_SEGMENTS as f32);
                            let x = self.geometry.width * (t * 1.18 - 0.09);
                            let y = self.geometry.height
                                * (0.48 - 0.24 * (t * PI).sin()
                                    + lane as f32 * 0.075
                                    + 0.045 * (t * TAU + lane as f32 * 0.8 - ripple).sin());
                            let p = bounds.origin + point(px(x), px(y));
                            if step == 0 {
                                path.move_to(p);
                            } else {
                                path.line_to(p);
                            }
                        }
                        if let Ok(path) = path.build() {
                            window.paint_path(path, color);
                        }
                    }
                }
            },
        )
        .absolute()
        .inset_0()
        .size_full()
    }
}

fn flight_clock(seconds: f32) -> (f32, f32) {
    let launch = smooth(segment(seconds, 0., FLIGHT_LAUNCH_SECONDS));
    let cruising = (seconds - FLIGHT_LAUNCH_SECONDS).max(0.);
    // Ease into a perpetual orbit once, so long waits never freeze or restart the entrance.
    let clock = cruising * smooth(segment(cruising, 0., CRUISE_BLEND_SECONDS));
    (launch, clock / CRUISE_CYCLE_SECONDS * TAU)
}
fn cruise_offset(phase: f32) -> Point<f32> {
    point(20. * phase.sin(), 10. * (phase.cos() - 1.))
}
fn mix_point(a: Point<f32>, b: Point<f32>, t: f32) -> Point<f32> {
    point(lerp(a.x, b.x, t), lerp(a.y, b.y, t))
}
fn cubic(points: [Point<f32>; 4], t: f32) -> Point<f32> {
    let s = 1. - t;
    point(
        s.powi(3) * points[0].x
            + 3. * s * s * t * points[1].x
            + 3. * s * t * t * points[2].x
            + t.powi(3) * points[3].x,
        s.powi(3) * points[0].y
            + 3. * s * s * t * points[1].y
            + 3. * s * t * t * points[2].y
            + t.powi(3) * points[3].y,
    )
}
