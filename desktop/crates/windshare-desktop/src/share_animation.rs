use crate::{
    artwork::Artwork,
    layout::{FlowLayout, task_width},
    motion::{Spirit, SpiritScene, SpiritState},
    share_timing::{
        FLIGHT_DURATION, LANDING_DURATION, ShareFrame, ease_out, lerp, segment, smooth,
    },
};
use gpui::{Div, Point, Window, div, point, prelude::*};
use std::f32::consts::PI;
use windshare_ui::LARGE_BUTTON_HEIGHT;

pub const SHARE_CHOICE_SECTION_GAP: f32 = 24.;
pub const SHARE_CHOICE_TITLE_HEIGHT: f32 = 36.;
const SHARE_CHOICE_CONTROLS_HEIGHT: f32 =
    2. * SHARE_CHOICE_SECTION_GAP + SHARE_CHOICE_TITLE_HEIGHT + LARGE_BUTTON_HEIGHT;

const LANDING_X_FRACTION: f32 = 0.68;
const LANDING_Y_FRACTION: f32 = 0.24;
const LANDED_SPIRIT_SIZE: f32 = 52.;
const CRUISING_SPIRIT_SIZE: f32 = 144.;
const PAPER_COUNT: u16 = 7;
const PAPER_ASSETS: [Artwork; 3] = [Artwork::Paper, Artwork::Photo, Artwork::Folder];

#[derive(Clone, Copy)]
pub struct ShareGeometry {
    pub width: f32,
    pub height: f32,
    initial_spirit_size: f32,
    initial_spirit_y: f32,
}
impl ShareGeometry {
    pub fn new(window: &Window, layout: FlowLayout) -> Self {
        Self {
            width: task_width(window),
            height: layout.stage_height(),
            initial_spirit_size: 156. * layout.scene_scale(),
            // The chooser is centered as a group; launch from the body's visual
            // center so layout edits do not create a jump at the first frame.
            initial_spirit_y: (layout.stage_height() - SHARE_CHOICE_CONTROLS_HEIGHT) / 2.
                + SpiritScene::body_center_offset_y(layout.scene_scale()),
        }
    }
    fn destination(self) -> Point<f32> {
        point(
            self.width * LANDING_X_FRACTION,
            self.height * LANDING_Y_FRACTION,
        )
    }
    fn flight_end(self) -> Point<f32> {
        self.destination() + point(-46., 28.)
    }
    fn paper_destination(self) -> Point<f32> {
        point(self.width / 2., self.height / 2.)
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
    pub fn content_visibility(self) -> f32 {
        self.reveal(0.24, 1.)
    }
    pub fn decorations_visible(self) -> bool {
        self.arrival_progress() < 1.
    }
    pub fn preparation_visibility(self) -> f32 {
        let t = self.flight_progress();
        smooth(segment(t, 0., 0.18)) * (1. - smooth(segment(t, 0.82, 1.)))
    }
    fn reveal(self, start: f32, end: f32) -> f32 {
        smooth(segment(self.arrival_progress(), start, end))
    }
    pub fn scenery(self, reduced_motion: bool) -> Div {
        div()
            .absolute()
            .inset_0()
            .size_full()
            .when(self.arrival_progress() < 1., |el| {
                el.child(self.wind_field())
                    .children((0..PAPER_COUNT).filter_map(|index| self.paper(index)))
            })
            .child(self.spirit(reduced_motion))
    }

    fn spirit_pose(self) -> (Point<f32>, f32) {
        let g = self.geometry;
        if self.landing() {
            let t = smooth(self.arrival_progress());
            (
                mix_point(g.flight_end(), g.destination(), t),
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
                        point(g.width * 0.5, g.initial_spirit_y),
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

    fn spirit(self, reduced_motion: bool) -> Div {
        let (center, size) = self.spirit_pose();
        let visibility = 1. - self.reveal(0.12, 0.72);
        div()
            .absolute()
            .inset_0()
            .opacity(visibility)
            .when(visibility > 0., |el| {
                el.child(Spirit::new(
                    "share-spirit",
                    SpiritState::Choreographed {
                        smile: self.reveal(0.02, 0.42) * self.frame.result_visibility,
                    },
                    center,
                    size,
                    reduced_motion,
                ))
            })
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
            let destination = g.paper_destination();
            (
                cubic(
                    [
                        end,
                        point(end.x + 45. - seed * 10., end.y + 95.),
                        point(destination.x + 100. + seed * 9., destination.y - 70.),
                        destination,
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
