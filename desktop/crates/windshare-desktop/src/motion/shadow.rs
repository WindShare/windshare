use gpui::{App, IntoElement, Window, prelude::*, px, rgba, svg};
use windshare_ui::{Theme, Tone};

const SHADOW_WIDTH: f32 = 85.;
const SHADOW_HEIGHT: f32 = 10.;
const SHADOW_LEFT: f32 = 132.6;
const SHADOW_TOP: f32 = 205.;
const SHADOW_LIGHT: u32 = 0x5484_6524;
const SHADOW_DARK: u32 = 0xafdf_c11c;
const LIGHT_OPACITY: f32 = 36. / 255.;
const DARK_OPACITY: f32 = 28. / 255.;

#[derive(IntoElement)]
struct GroundShadow {
    tone: Tone,
    scale: f32,
}

pub fn ground_shadow(tone: Tone, scale: f32) -> impl IntoElement {
    GroundShadow { tone, scale }
}

impl RenderOnce for GroundShadow {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let theme = Theme::current(window, cx);
        let color = match (self.tone, theme) {
            (Tone::Mint, Theme::Light) => rgba(SHADOW_LIGHT).into(),
            (Tone::Mint, Theme::Dark) => rgba(SHADOW_DARK).into(),
            (_, Theme::Light) => self.tone.accent(theme).opacity(LIGHT_OPACITY),
            (_, Theme::Dark) => self.tone.accent(theme).opacity(DARK_OPACITY),
        };
        // Match the web study's elliptical radial falloff. A cached mask at its
        // final size keeps the soft edge without spreading a box shadow thinner.
        svg()
            .data(include_bytes!("../../assets/spirit-ground.svg"))
            .absolute()
            .left(px(SHADOW_LEFT * self.scale))
            .top(px(SHADOW_TOP * self.scale))
            .w(px(SHADOW_WIDTH * self.scale))
            .h(px(SHADOW_HEIGHT * self.scale))
            .text_color(color)
    }
}
