use gpui::{Div, div, prelude::*, px};
use windshare_ui::{Icon, Theme, column, icon};

const SCAN_FRAME_SIDE: f32 = 112.;
const SCAN_CORNER_SIDE: f32 = 24.;
const SCAN_CORNER_RADIUS: f32 = 7.;
const WAITING_ICON_SIDE: f32 = 42.;

pub(super) fn waiting_qr(theme: Theme) -> Div {
    column()
        .absolute()
        .inset_0()
        .size_full()
        .items_center()
        .justify_center()
        .child(
            column()
                .relative()
                .size(px(SCAN_FRAME_SIDE))
                .items_center()
                .justify_center()
                .children((0..4).map(|corner| scan_corner(theme, corner)))
                .child(
                    icon(Icon::QrCode, WAITING_ICON_SIDE)
                        .text_color(theme.accent_text())
                        .opacity(0.55),
                ),
        )
}
fn scan_corner(theme: Theme, corner: u8) -> Div {
    div()
        .absolute()
        .size(px(SCAN_CORNER_SIDE))
        .border_color(theme.accent_text().opacity(0.25))
        .when(corner == 0, |el| {
            el.top_0()
                .left_0()
                .border_t_2()
                .border_l_2()
                .rounded_tl(px(SCAN_CORNER_RADIUS))
        })
        .when(corner == 1, |el| {
            el.top_0()
                .right_0()
                .border_t_2()
                .border_r_2()
                .rounded_tr(px(SCAN_CORNER_RADIUS))
        })
        .when(corner == 2, |el| {
            el.bottom_0()
                .right_0()
                .border_b_2()
                .border_r_2()
                .rounded_br(px(SCAN_CORNER_RADIUS))
        })
        .when(corner == 3, |el| {
            el.bottom_0()
                .left_0()
                .border_b_2()
                .border_l_2()
                .rounded_bl(px(SCAN_CORNER_RADIUS))
        })
}
