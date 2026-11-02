use crate::{
    artwork::Artwork,
    layout::{FlowLayout, task_width},
    motion::{Spirit, SpiritMood},
    share_timing::{
        FLIGHT_DURATION, LANDING_DURATION, ShareFrame, ease_out, lerp, segment, smooth,
    },
};
use gpui::{Div, Point, Window, div, point, prelude::*, px};
use std::f32::consts::PI;
use windshare_ui::{Theme, Tone, vector_sprite};

const READY_WIDTH: f32 = 620.;
const READY_HEIGHT: f32 = 352.;
pub const SHARE_LINK_HEIGHT: f32 = 78.;
const SHARE_LINK_TOP: f32 = 116.;
const LANDED_SPIRIT_SIZE: f32 = 92.;
const CRUISING_SPIRIT_SIZE: f32 = 144.;
const PAPER_COUNT: u16 = 7;
const SURROUND_PAPER_WIDTH: f32 = 23.;
const SURROUND_PAPER_HEIGHT: f32 = 27.;
const PAPER_ASSETS: [Artwork; 3] = [Artwork::Paper, Artwork::Photo, Artwork::Folder];

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
    pub fn scenery(self, theme: Theme, reduced_motion: bool) -> Div {
        div()
            .absolute()
            .inset_0()
            .size_full()
            .when(self.arrival_progress() < 1., |el| {
                el.child(self.wind_field())
                    .children((0..PAPER_COUNT).filter_map(|index| self.paper(index)))
            })
            .child(self.spirit(theme, reduced_motion))
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

    fn spirit(self, theme: Theme, reduced_motion: bool) -> Div {
        let (center, size) = self.spirit_pose();
        let interactive = self.arrival_progress() >= 1. && self.frame.result_visibility >= 1.;
        let smile = self.reveal(0.02, 0.42) * self.frame.result_visibility;
        let local_scale = size / 156.;
        let breeze_offset = 6. * local_scale;
        let surround = self.reveal(0.55, 1.);
        let mut layer = div()
            .absolute()
            .inset_0()
            .when(surround > 0., |el| {
                el.child(
                    vector_sprite(
                        "brand/breeze.svg",
                        gpui::size(px(340.), px(176.)),
                        point(px(center.x), px(center.y + breeze_offset)),
                        local_scale,
                    )
                    .text_color(Tone::Mint.accent(theme))
                    .opacity(surround),
                )
            })
            // Choreography owns the pose until landing and expression reveal finish.
            // Only the settled character gets a stationary, accessible hit target.
            .when(interactive, |el| {
                el.child(Spirit::new(
                    "share-ready-spirit",
                    SpiritMood::Happy,
                    self.geometry.destination(),
                    LANDED_SPIRIT_SIZE,
                    reduced_motion,
                ))
            })
            .when(!interactive && smile < 1., |el| {
                el.child(
                    div()
                        .absolute()
                        .inset_0()
                        .child(Artwork::Spirit.sprite(center, size))
                        .opacity(1. - smile),
                )
            })
            .when(!interactive && smile > 0., |el| {
                el.child(
                    div()
                        .absolute()
                        .inset_0()
                        .child(Artwork::Happy.sprite(center, size))
                        .opacity(smile),
                )
            });
        // Zero-opacity SVG layers still incur layout and atlas work in GPUI.
        // Keep their lifetime aligned with the visible part of the choreography.
        if surround <= 0. {
            return layer;
        }
        for (index, asset) in PAPER_ASSETS.into_iter().enumerate() {
            let (x, y) = [(-82., -8.), (77., -29.), (76., 38.)][index];
            let drift = self.frame.breath * if index == 1 { -2. } else { 2. };
            layer = layer.child(
                div()
                    .absolute()
                    .inset_0()
                    .child(asset.sprite(
                        point(
                            center.x + x + SURROUND_PAPER_WIDTH / 2.,
                            center.y + y + drift + SURROUND_PAPER_HEIGHT / 2.,
                        ),
                        SURROUND_PAPER_WIDTH,
                    ))
                    .opacity(surround),
            );
        }
        layer
    }

    #[allow(
        clippy::suboptimal_flops,
        reason = "Control-point formulas keep the per-paper spatial and timing offsets visible."
    )]
    fn paper(self, index: u16) -> Option<Div> {
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
        (opacity > 0.).then(|| {
            div()
                .absolute()
                .inset_0()
                .size_full()
                .opacity(opacity)
                .child(PAPER_ASSETS[usize::from(index) % PAPER_ASSETS.len()].sprite(center, size))
        })
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
        let (head, strength) = self.wind_pose();
        crate::motion::wind_field(head, strength)
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
