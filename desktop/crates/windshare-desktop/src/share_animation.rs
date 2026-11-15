use crate::{
    artwork::Artwork,
    layout::{FlowLayout, task_width},
    motion::{SpiritScene, SpiritState, ground_shadow},
    share_timing::{
        FLIGHT_DURATION, LANDING_DURATION, STORY_DURATION, ShareFrame, ease_out, lerp, segment,
        smooth,
    },
};
use gpui::{Div, Point, Window, div, point, prelude::*, px, relative};
use std::f32::consts::PI;
use windshare_ui::{LARGE_BUTTON_HEIGHT, Tone};

pub const SHARE_CHOICE_SECTION_GAP: f32 = 24.;
pub const SHARE_CHOICE_TITLE_HEIGHT: f32 = 36.;
const SHARE_CHOICE_CONTROLS_HEIGHT: f32 =
    SHARE_CHOICE_TITLE_HEIGHT + SHARE_CHOICE_SECTION_GAP + LARGE_BUTTON_HEIGHT;

const LANDING_X_FRACTION: f32 = 0.68;
const LANDING_Y_FRACTION: f32 = 0.24;
const LANDED_SPIRIT_SIZE: f32 = 52.;
const SPIRIT_FLIGHT_EXPANSION: f32 = 64.;
const WIND_HEAD_START: f32 = 0.18;
const WIND_HEAD_END: f32 = 1.75;
const PAPER_COUNT: u16 = 7;
const PAPER_ASSETS: [Artwork; 3] = [Artwork::Paper, Artwork::Photo, Artwork::Folder];

#[derive(Clone, Copy)]
pub struct ShareGeometry {
    pub width: f32,
    pub height: f32,
    scale: f32,
    initial_spirit_size: f32,
    initial_spirit_y: f32,
}
impl ShareGeometry {
    pub fn new(window: &Window, layout: FlowLayout) -> Self {
        let scale = layout.scene_scale();
        let height = layout
            .stage_height()
            .max(SpiritScene::height(scale) + SHARE_CHOICE_CONTROLS_HEIGHT);
        Self {
            width: task_width(window),
            height,
            scale,
            initial_spirit_size: SpiritScene::body_size(scale),
            // Layout and the flight share the same origin, including the new
            // scene height; the first frame must preserve the visible pose.
            initial_spirit_y: (height - SHARE_CHOICE_CONTROLS_HEIGHT) / 2.
                + SpiritScene::body_center_offset_y(scale),
        }
    }
    pub fn spirit_space(self) -> Div {
        div()
            .w(px(SpiritScene::width(self.scale)))
            .h(px(SpiritScene::height(self.scale)))
            .flex_shrink_0()
    }

    fn scene_origin(self) -> Div {
        let chooser_height = SpiritScene::height(self.scale) + SHARE_CHOICE_CONTROLS_HEIGHT;
        div()
            .absolute()
            .left(relative(0.5))
            .top(relative(0.5))
            .ml(px(-SpiritScene::width(self.scale) / 2.))
            .mt(px(-chooser_height / 2.))
    }

    pub fn spirit(self, state: SpiritState, reduced_motion: bool) -> Div {
        self.scene_origin().child(SpiritScene::new(
            "share-spirit",
            state,
            Tone::Mint,
            self.scale,
            reduced_motion,
        ))
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
    fn landing(self) -> bool {
        self.frame.story >= FLIGHT_DURATION
    }
    fn story_progress(self) -> f32 {
        self.frame.story.as_secs_f32() / STORY_DURATION.as_secs_f32()
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
                el.child(
                    self.geometry
                        .scene_origin()
                        .opacity(1. - smooth(segment(self.flight_progress(), 0., 0.25)))
                        .child(ground_shadow(Tone::Mint, self.geometry.scale)),
                )
                .child(self.wind_field())
                .children((0..PAPER_COUNT).filter_map(|index| self.paper(index)))
            })
            .child(self.spirit(reduced_motion))
    }

    fn spirit_pose(self) -> (Point<f32>, f32) {
        let g = self.geometry;
        // Content phases must not stop and restart the character. One trajectory
        // carries position and scale through the reveal with continuous velocity.
        let t = smooth(self.story_progress());
        let end = g.destination();
        let expansion = SPIRIT_FLIGHT_EXPANSION * (PI * t).sin().powi(2);
        (
            cubic(
                [
                    point(g.width * 0.5, g.initial_spirit_y),
                    point(g.width * 0.14, g.height * 0.63),
                    point(end.x - 110., end.y + 36.),
                    end,
                ],
                t,
            ),
            lerp(g.initial_spirit_size, LANDED_SPIRIT_SIZE, t) + expansion,
        )
    }

    fn spirit(self, reduced_motion: bool) -> impl IntoElement {
        let g = self.geometry;
        let (center, size) = self.spirit_pose();
        let origin = point(
            (g.width - SpiritScene::width(g.scale)) / 2.,
            (g.height - SpiritScene::height(g.scale) - SHARE_CHOICE_CONTROLS_HEIGHT) / 2.,
        );
        let visibility = 1. - self.reveal(0.12, 0.72);
        // Preserve the entire keyed component, including its layout origin.
        // Only the vector pose changes as the character follows the flight path.
        g.scene_origin()
            .opacity(visibility)
            .when(visibility > 0., |el| {
                el.child(
                    SpiritScene::new(
                        "share-spirit",
                        SpiritState::Choreographed {
                            progress: self.story_progress(),
                            smile: self.reveal(0.02, 0.42) * self.frame.link.content_visibility(),
                        },
                        Tone::Mint,
                        g.scale,
                        reduced_motion,
                    )
                    .with_pose(center - origin, size),
                )
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
        // Phase-local easing would stop the visible ribbon at the flight/landing
        // boundary. Keep its travel on the same uninterrupted clock as the spirit.
        let head = lerp(
            WIND_HEAD_START,
            WIND_HEAD_END,
            smooth(self.story_progress()),
        );
        let strength = if self.landing() {
            0.8 * (1. - smooth(segment(self.arrival_progress(), 0., 0.8)))
        } else {
            let t = self.flight_progress();
            smooth(segment(t, 0., 0.14)) * lerp(1., 0.8, segment(t, 0.9, 1.))
        };
        (head, strength)
    }
    fn wind_field(self) -> impl IntoElement {
        let (head, strength) = self.wind_pose();
        crate::motion::wind_field(head, strength)
    }
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
