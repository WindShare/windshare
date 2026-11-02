use crate::{
    layout::{FlowLayout, task_width},
    share_timing::{
        FLIGHT_DURATION, LANDING_DURATION, ShareFrame, ease_out, lerp, segment, smooth,
    },
};
use gpui::{Div, PathBuilder, Point, Window, canvas, div, img, point, prelude::*, px};
use std::f32::consts::{PI, TAU};
use windshare_ui::{Tone, vector_sprite};

const READY_WIDTH: f32 = 620.;
const READY_HEIGHT: f32 = 352.;
pub const SHARE_LINK_HEIGHT: f32 = 78.;
const SHARE_LINK_TOP: f32 = 116.;
const LANDED_SPIRIT_SIZE: f32 = 92.;
const CRUISING_SPIRIT_SIZE: f32 = 144.;
const PAPER_COUNT: u16 = 7;
const WIND_SEGMENTS: u16 = 48;
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
        self.destination() + point(-46., 28.)
    }
    pub fn link_top(self) -> f32 {
        self.content_top + SHARE_LINK_TOP
    }
    fn link_anchor(self) -> Point<f32> {
        point(
            self.content_left + 30.,
            self.link_top() + SHARE_LINK_HEIGHT / 2.,
        )
    }
}

#[derive(Clone, Copy)]
pub struct ShareScene {
    pub geometry: ShareGeometry,
    pub frame: ShareFrame,
}
impl ShareScene {
    pub const fn new(geometry: ShareGeometry, frame: ShareFrame) -> Self {
        Self { geometry, frame }
    }
    pub fn landing(self) -> bool {
        self.frame.story >= FLIGHT_DURATION
    }
    fn flight_progress(self) -> f32 {
        (self.frame.story.as_secs_f32() / FLIGHT_DURATION.as_secs_f32()).min(1.)
    }
    fn arrival_progress(self) -> f32 {
        (self
            .frame
            .story
            .saturating_sub(FLIGHT_DURATION)
            .as_secs_f32()
            / LANDING_DURATION.as_secs_f32())
        .min(1.)
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
        self.reveal(0., 0.22)
    }
    pub fn summary_visibility(self) -> f32 {
        self.reveal(0.24, 0.72)
    }
    pub fn footer_visibility(self) -> f32 {
        self.reveal(0.42, 0.86)
    }
    pub fn preparation_visibility(self) -> f32 {
        let t = self.flight_progress();
        smooth(segment(t, 0., 0.18)) * (1. - smooth(segment(t, 0.82, 1.)))
    }
    fn reveal(self, start: f32, end: f32) -> f32 {
        smooth(segment(self.arrival_progress(), start, end))
    }
    pub fn scenery(self) -> Div {
        div()
            .absolute()
            .inset_0()
            .size_full()
            .when(self.arrival_progress() < 1., |el| {
                el.child(self.wind_field())
                    .children((0..PAPER_COUNT).map(|index| self.paper(index)))
            })
            .child(self.spirit())
    }

    fn spirit_pose(self) -> (Point<f32>, f32) {
        let g = self.geometry;
        if self.landing() {
            let t = smooth(self.arrival_progress());
            (
                mix_point(g.flight_end(), g.destination(), t) + point(0., self.frame.breath * 2.),
                lerp(CRUISING_SPIRIT_SIZE, LANDED_SPIRIT_SIZE, t),
            )
        } else {
            let t = self.flight_progress();
            let end = g.flight_end();
            let cruising_size = lerp(g.initial_spirit_size, CRUISING_SPIRIT_SIZE, smooth(t));
            let flight_stretch = 64. * (PI * t).sin();
            (
                cubic(
                    [
                        point(g.width * 0.5, g.height * 0.235),
                        point(g.width * 0.14, g.height * 0.63),
                        point(end.x - 110., end.y + 36.),
                        end,
                    ],
                    smooth(t),
                ),
                cruising_size + flight_stretch,
            )
        }
    }

    fn spirit(self) -> Div {
        let (center, size) = self.spirit_pose();
        let smile = self.reveal(0.02, 0.42) * self.frame.result_visibility;
        let local_scale = size / 156.;
        let breeze_offset = 6. * local_scale;
        let surround = self.reveal(0.55, 1.);
        let mut layer = div()
            .absolute()
            .inset_0()
            .child(
                vector_sprite(
                    "brand/breeze.svg",
                    gpui::size(px(340.), px(176.)),
                    point(px(center.x), px(center.y + breeze_offset)),
                    local_scale,
                )
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
            let drift = self.frame.breath * if index == 1 { -2. } else { 2. };
            layer = layer.child(
                img(asset)
                    .absolute()
                    .left(px(center.x + x))
                    .top(px(center.y + y + drift))
                    .w(px(23.))
                    .h(px(27.))
                    .opacity(surround),
            );
        }
        layer
    }

    #[allow(
        clippy::suboptimal_flops,
        reason = "Control-point formulas keep the per-paper spatial and timing offsets visible."
    )]
    fn paper(self, index: u16) -> Div {
        let g = self.geometry;
        let seed = f32::from(index);
        let (center, size, opacity) = if self.landing() {
            let p = ease_out(segment(
                self.arrival_progress(),
                seed * 0.018,
                0.48 + seed * 0.025,
            ));
            let end = g.flight_end();
            let link = g.link_anchor();
            (
                cubic(
                    [
                        end,
                        point(end.x + 45. - seed * 10., end.y + 95.),
                        point(link.x + 100. + seed * 9., link.y - 70.),
                        link,
                    ],
                    p,
                ),
                lerp(27., 10., p),
                1. - smooth(segment(p, 0.64, 1.)),
            )
        } else {
            let t = self.flight_progress();
            let p = smooth(segment(t, seed * 0.028, 0.88 + seed * 0.018));
            let flank = if index.is_multiple_of(2) { -0.12 } else { 1.10 };
            (
                cubic(
                    [
                        point(g.width * (0.39 + seed * 0.035), g.height * 0.57),
                        point(g.width * flank, g.height * (0.62 - seed * 0.045)),
                        point(
                            g.width * (0.12 + seed * 0.115),
                            g.height * (-0.25 + seed * 0.035),
                        ),
                        g.flight_end(),
                    ],
                    p,
                ),
                lerp(54. + seed * 3., 27., p),
                smooth(segment(t, 0.01, 0.13)),
            )
        };
        div()
            .absolute()
            .left(px(center.x - size / 2.))
            .top(px(center.y - size * 0.57))
            .w(px(size))
            .h(px(size * 1.14))
            .opacity(opacity)
            .child(img(PAPER_ASSETS[usize::from(index) % PAPER_ASSETS.len()]).size_full())
    }

    fn wind_pose(self) -> (f32, f32) {
        if self.landing() {
            let t = self.arrival_progress();
            (
                lerp(1.15, 1.75, smooth(t)),
                0.8 * (1. - smooth(segment(t, 0., 0.8))),
            )
        } else {
            let t = self.flight_progress();
            (
                lerp(0.18, 1.15, smooth(t)),
                smooth(segment(t, 0., 0.14)) * lerp(1., 0.8, segment(t, 0.9, 1.)),
            )
        }
    }
    fn wind_field(self) -> impl IntoElement {
        canvas(
            |_, _, _| (),
            move |bounds, (), window, _| {
                let (head, strength) = self.wind_pose();
                for (lane, tone) in (0_u16..).zip([Tone::Mint, Tone::Lavender, Tone::Peach]) {
                    for (width, color) in [
                        (17., tone.surface().opacity(strength * 0.78)),
                        (1.5, tone.accent().opacity(strength * 0.72)),
                    ] {
                        let mut path = PathBuilder::stroke(px(width));
                        let begin = (head - 0.88).max(0.);
                        for step in 0..=WIND_SEGMENTS {
                            let t = lerp(begin, head, f32::from(step) / f32::from(WIND_SEGMENTS));
                            let travel = t * 1.18;
                            let cycle_angle = t * TAU;
                            let lane_phase = f32::from(lane) * 0.8;
                            let wave = (cycle_angle + lane_phase).sin() * 0.045;
                            let arch = (t * PI).sin() * 0.24;
                            let lane_offset = f32::from(lane) * 0.075;
                            let x = self.geometry.width * (travel - 0.09);
                            let y = self.geometry.height * (0.48 - arch + lane_offset + wave);
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

fn mix_point(a: Point<f32>, b: Point<f32>, t: f32) -> Point<f32> {
    point(lerp(a.x, b.x, t), lerp(a.y, b.y, t))
}
#[allow(
    clippy::suboptimal_flops,
    reason = "Keep the Bernstein weights visible instead of nesting fused coordinate operations."
)]
fn cubic(points: [Point<f32>; 4], t: f32) -> Point<f32> {
    let [start, first_control, second_control, end] = points;
    let remaining = 1. - t;
    let start_weight = remaining.powi(3);
    let first_control_weight = 3. * remaining * remaining * t;
    let second_control_weight = 3. * remaining * t * t;
    let end_weight = t.powi(3);

    point(
        start_weight * start.x
            + first_control_weight * first_control.x
            + second_control_weight * second_control.x
            + end_weight * end.x,
        start_weight * start.y
            + first_control_weight * first_control.y
            + second_control_weight * second_control.y
            + end_weight * end.y,
    )
}
