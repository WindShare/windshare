use super::qr_slot::{QR_FRAME_RADIUS, QrSlot};
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::Action,
};
use gpui::{Div, Role, prelude::*, px};
use windshare_ui::{Icon, Theme, column, icon, row};

const QR_CAPTION_PADDING: f32 = 6.;

impl Desktop {
    /// A slot with its caption. Together they enlarge the code once the slot has one.
    pub(crate) fn share_qr(theme: Theme, share_id: usize, slot: QrSlot, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let ready = slot.has_code();
        column().flex_shrink_0().child(
            column()
                .id(("share-qr", share_id))
                .items_center()
                .gap(px(8.))
                .rounded(px(QR_FRAME_RADIUS))
                .border_1()
                .border_color(gpui::transparent_black())
                .text_size(px(13.))
                .text_color(theme.muted_text())
                .when(ready, |el| {
                    el.role(Role::Button)
                        .aria_label(locale.text(Text::EnlargeQr))
                        .focusable()
                        .tab_index(0)
                        .cursor_pointer()
                        .hover(|s| s.text_color(theme.accent_text()))
                        .focus_visible(|s| s.border_color(theme.focus_ring()))
                        .on_click(cx.listener(move |this, _, window, cx| {
                            cx.stop_propagation();
                            this.dispatch(Action::EnlargeQr(share_id), window, cx);
                        }))
                })
                .child(slot)
                .child(
                    row()
                        .gap(px(7.))
                        .py(px(QR_CAPTION_PADDING))
                        .child(locale.text(Text::ScanToReceive))
                        .child(
                            icon(Icon::Expand, 14.)
                                .text_color(theme.muted_text())
                                .opacity(if ready { 1. } else { 0. }),
                        ),
                ),
        )
    }
}
