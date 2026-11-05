use crate::artwork::Artwork;
use gpui::{Animation, AnimationExt, Div, FontWeight, Hsla, div, point, prelude::*, px, rgb};
use std::time::Duration;
use windshare_ui::{FONT, Icon, Theme, column, icon, row};

#[derive(Clone, Copy)]
pub(super) struct Finish {
    pub edge: Hsla,
    pub pearl: Hsla,
    pub mint: Hsla,
    pub foil: Hsla,
    pub ink: Hsla,
    pub sky: Hsla,
    pub blush: Hsla,
}

impl Finish {
    pub fn new(theme: Theme) -> Self {
        let tint = |light, dark| {
            rgb(match theme {
                Theme::Light => light,
                Theme::Dark => dark,
            })
            .into()
        };
        Self {
            edge: tint(0x8b_af_99, 0x58_7b_65),
            pearl: tint(0xff_f9_ea, 0x2a_39_30),
            mint: tint(0xb1_d4_bc, 0x2c_55_40),
            foil: tint(0xa0_7c_3d, 0xd8_bc_7d),
            ink: tint(0x27_4b_38, 0xe7_ef_df),
            sky: tint(0xe2_ef_ec, 0x18_32_2d),
            blush: tint(0xf1_df_c6, 0x44_39_2b),
        }
    }
}

pub fn brand_mark(width: f32) -> Div {
    div()
        .relative()
        .size(px(width))
        .flex_shrink_0()
        .child(Artwork::Super.sprite(point(width / 2., width / 2.), width))
}

pub(super) fn heading(theme: Theme, title_size: f32) -> Div {
    let finish = Finish::new(theme);
    column()
        .gap(px(22.))
        .child(
            row()
                .gap(px(10.))
                .child(icon(Icon::Spark, 17.).text_color(finish.foil))
                .child(
                    theme
                        .label("WINDSHARE", 12.)
                        .font_family(FONT)
                        .font_weight(FontWeight::SEMIBOLD)
                        .text_color(finish.ink),
                ),
        )
        .child(
            theme
                .heading("SuperShare", title_size)
                .font_family(FONT)
                .line_height(px(title_size * 1.12))
                .text_color(finish.ink),
        )
}

pub(super) fn price(theme: Theme, amount: &'static str, large: bool) -> Div {
    row()
        .items_baseline()
        .gap(px(5.))
        .child(
            theme
                .label("$", if large { 25. } else { 17. })
                .font_family(FONT),
        )
        .child(
            theme
                .heading(amount, if large { 52. } else { 26. })
                .font_family(FONT),
        )
        .child(theme.caption("USD").font_family(FONT).ml(px(7.)))
}

pub(super) fn progress(theme: Theme, still: bool) -> Div {
    let track = div()
        .w(px(120.))
        .h(px(3.))
        .rounded_full()
        .bg(theme.border())
        .overflow_hidden()
        .relative();
    let runner = div()
        .absolute()
        .h_full()
        .w(px(40.))
        .rounded_full()
        .bg(theme.accent_text());
    if still {
        track.child(runner.left(px(40.)))
    } else {
        track.child(runner.with_animation(
            "supershare-progress",
            Animation::new(Duration::from_millis(1200)).repeat(),
            |el, delta| el.left(px(160.0_f32.mul_add(delta, -40.))),
        ))
    }
}
