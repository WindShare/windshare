pub mod paper;
mod placement;
mod shadow;
mod share;
mod spirit;
pub mod whirl;
pub use placement::{SpiritAnchor, SpiritPlacement};
pub use shadow::ground_shadow;
pub use share::ShareAnimation;
pub use spirit::Spirit;

use gpui::{
    Animation, AnimationExt, AnyElement, App, Canvas, Div, ElementId, IntoElement, Point, Window,
    canvas, div, ease_out_quint, point, prelude::*, px,
};
use std::time::Duration;
use windshare_ui::{Icon, Theme, Tone, icon, row};

const SCENE_WIDTH: f32 = 340.;
const SCENE_HEIGHT: f32 = 250.;
const SPIRIT_SIZE: f32 = 190.;
const SPIRIT_TOP: f32 = 25.;

#[derive(Clone, Copy, PartialEq, Eq)]
pub enum SpiritMood {
    Idle,
    Receiving,
    Happy,
}

#[derive(IntoElement)]
pub struct SpiritScene {
    id: ElementId,
    mood: SpiritMood,
    tone: Tone,
    scale: f32,
    center: Point<f32>,
    body_width: f32,
    reduced_motion: bool,
    anchor: Option<SpiritAnchor>,
}

impl SpiritScene {
    pub fn new(
        id: impl Into<ElementId>,
        mood: SpiritMood,
        tone: Tone,
        scale: f32,
        reduced_motion: bool,
    ) -> Self {
        Self {
            id: id.into(),
            mood,
            tone,
            scale,
            center: Self::body_center(scale),
            body_width: Self::body_size(scale),
            reduced_motion,
            anchor: None,
        }
    }

    pub fn anchor(mut self, anchor: &SpiritAnchor) -> Self {
        self.anchor = Some(anchor.clone());
        self
    }

    pub const fn width(scale: f32) -> f32 {
        SCENE_WIDTH * scale
    }

    pub const fn height(scale: f32) -> f32 {
        SCENE_HEIGHT * scale
    }

    pub const fn body_size(scale: f32) -> f32 {
        SPIRIT_SIZE * scale
    }

    /// The resting spirit's center, measured from the scene's corner.
    pub fn body_center(scale: f32) -> Point<f32> {
        point(
            SCENE_WIDTH / 2. * scale,
            (SPIRIT_TOP + SPIRIT_SIZE / 2.) * scale,
        )
    }
}

/// A scene whose spirit is painted elsewhere: its footprint and the shadow it left.
pub fn vacated_scene(tone: Tone, scale: f32) -> Div {
    div()
        .relative()
        .w(px(SpiritScene::width(scale)))
        .h(px(SpiritScene::height(scale)))
        .flex_shrink_0()
        .child(ground_shadow(tone, scale))
}

/// Redraws the enclosing view every frame for as long as it stays in the tree.
/// Frames can only be requested while drawing, so a view whose content is a
/// function of the clock mounts this instead of calling from `render`.
pub fn frame_ticker() -> Canvas<()> {
    canvas(
        |_, window, _| {
            if window.is_visible() && window.is_window_active() {
                window.request_animation_frame();
            }
        },
        |_, (), _, _| {},
    )
    .absolute()
    .size_0()
}

pub fn spirit_scene(
    id: impl Into<ElementId>,
    mood: SpiritMood,
    tone: Tone,
    reduced_motion: bool,
) -> SpiritScene {
    scaled_spirit(id, mood, tone, 1., reduced_motion)
}

pub fn scaled_spirit(
    id: impl Into<ElementId>,
    mood: SpiritMood,
    tone: Tone,
    scale: f32,
    reduced_motion: bool,
) -> SpiritScene {
    SpiritScene::new(id, mood, tone, scale, reduced_motion)
}

impl RenderOnce for SpiritScene {
    fn render(self, _: &mut Window, _: &mut App) -> impl IntoElement {
        div()
            .id(self.id)
            .relative()
            .w(px(Self::width(self.scale)))
            .h(px(Self::height(self.scale)))
            .flex_shrink_0()
            .child(ground_shadow(self.tone, self.scale))
            .child(
                Spirit::new(
                    "body",
                    self.mood,
                    self.center,
                    self.body_width,
                    self.reduced_motion,
                )
                .anchor(self.anchor),
            )
    }
}

pub fn lerp(a: f32, b: f32, t: f32) -> f32 {
    (b - a).mul_add(t, a)
}

pub fn reassurance(theme: Theme, symbol: Icon, text: &'static str) -> Div {
    row()
        .gap(px(7.))
        .child(icon(symbol, 16.).text_color(theme.muted_text()))
        .child(theme.caption(text))
}

pub fn reveal(content: Div, id: impl Into<ElementId>, still: bool) -> AnyElement {
    content
        .with_animation(
            id,
            Animation::new(Duration::from_millis(if still { 1 } else { 300 }))
                .with_easing(ease_out_quint()),
            |el, delta| el.opacity(delta).relative().top(px((1. - delta) * 7.)),
        )
        .into_any_element()
}
