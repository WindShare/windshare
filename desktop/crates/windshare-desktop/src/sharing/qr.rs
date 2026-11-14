use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::{Action, Share, ShareState},
    share_timing::{LinkPresentation, lerp},
};
use gpui::{Div, Role, div, prelude::*, px};
use windshare_ui::{Icon, Theme, column, icon, row};

pub(super) const QR_INLINE_SIDE: f32 = 288.;
pub(super) const QR_FRAME_SIDE: f32 = QR_INLINE_SIDE + 2.;
const QR_FRAME_RADIUS: f32 = 14.;
const QR_CAPTION_PADDING: f32 = 6.;
const SCAN_FRAME_SIDE: f32 = 112.;
const SCAN_CORNER_SIDE: f32 = 24.;
const SCAN_CORNER_RADIUS: f32 = 7.;
const WAITING_ICON_SIDE: f32 = 42.;

impl Desktop {
    pub(crate) fn share_qr(
        theme: Theme,
        share: &Share,
        presentation: LinkPresentation,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let link = share.usable_link();
        let code = link.and_then(|link| link.qr().ok());
        let ready = code.is_some();
        let waiting = link.is_none() || presentation.waiting_visibility() > 0.;
        let unavailable = code.is_none() && !matches!(share.state, ShareState::Preparing);
        let frame = column()
            .relative()
            // Reserve the largest inline size before encoding. Link density must not
            // resize the card or switch its columns at the moment it becomes usable.
            .size(px(QR_FRAME_SIDE))
            .flex_shrink_0()
            .border_1()
            .border_color(theme.border())
            .rounded(px(QR_FRAME_RADIUS))
            .overflow_hidden()
            .bg(theme.canvas())
            .when(waiting && !unavailable, |el| {
                el.child(waiting_qr(theme).opacity(presentation.waiting_visibility()))
            })
            .when_some(code, |el, code| {
                // Keep modules at their final physical-pixel size throughout the
                // reveal; scaling or sweeping a mask delays reliable scanning.
                el.child(
                    div()
                        .absolute()
                        .inset_0()
                        .opacity(presentation.qr_visibility())
                        .child(code.view(QR_INLINE_SIDE).rounded(px(QR_FRAME_RADIUS - 1.))),
                )
            })
            .when(unavailable, |el| {
                el.child(
                    column()
                        .size_full()
                        .items_center()
                        .justify_center()
                        .gap(px(12.))
                        .child(icon(Icon::QrCode, WAITING_ICON_SIDE).text_color(theme.muted_text()))
                        .child(theme.caption(locale.text(Text::QrUnavailable))),
                )
            });
        column().flex_shrink_0().child(
            column()
                .id(("share-qr", share.id))
                .items_center()
                .gap(px(8.))
                .rounded(px(QR_FRAME_RADIUS))
                .border_1()
                .border_color(gpui::transparent_black())
                .text_size(px(13.))
                .text_color(theme.muted_text())
                .when(ready, |el| {
                    let id = share.id;
                    el.role(Role::Button)
                        .aria_label(locale.text(Text::EnlargeQr))
                        .focusable()
                        .tab_index(0)
                        .cursor_pointer()
                        .hover(|s| s.text_color(theme.accent_text()))
                        .focus_visible(|s| s.border_color(theme.focus_ring()))
                        .on_click(cx.listener(move |this, _, window, cx| {
                            cx.stop_propagation();
                            this.dispatch(Action::EnlargeQr(id), window, cx);
                        }))
                })
                .child(frame)
                .child(
                    row()
                        .gap(px(7.))
                        .py(px(QR_CAPTION_PADDING))
                        .opacity(lerp(0.65, 1., presentation.content_visibility()))
                        .child(locale.text(Text::ScanToReceive))
                        .child(
                            icon(Icon::Expand, 14.)
                                .text_color(theme.muted_text())
                                .opacity(if ready {
                                    presentation.content_visibility()
                                } else {
                                    0.
                                }),
                        ),
                ),
        )
    }
}

fn waiting_qr(theme: Theme) -> Div {
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
