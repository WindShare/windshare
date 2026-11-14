use crate::{
    desktop::Desktop,
    flow_state::CopyFeedback,
    localization::{Text, language},
    model::{Action, Overlay, Share},
};
use gpui::{Div, Role, Stateful, Window, div, prelude::*, px};
use windshare_ui::{Button, ButtonStyle, FONT, Icon, Theme, column, icon, row};

pub(super) const LINK_PANEL_WIDTH: f32 = 700.;
pub(super) const LINK_COLUMN_GAP: f32 = 32.;
pub(super) const LINK_TEXT_MIN_WIDTH: f32 = 280.;
const QR_INLINE_MIN_SIDE: f32 = 240.;
const QR_INLINE_MAX_SIDE: f32 = 288.;
const QR_MODULE_SIDE: f32 = 4.;
const QR_FRAME_BORDER: f32 = 1.;

pub(super) fn inline_qr_side(share: Option<&Share>) -> f32 {
    share
        .and_then(Share::usable_link)
        .and_then(|link| link.qr().ok())
        .map_or(QR_INLINE_MIN_SIDE, |code| {
            code.side_for_modules(QR_MODULE_SIDE)
                .clamp(QR_INLINE_MIN_SIDE, QR_INLINE_MAX_SIDE)
        })
}

pub(super) fn link_columns_fit(width: f32, padding: f32, share: Option<&Share>) -> bool {
    padding.mul_add(-2., width.min(LINK_PANEL_WIDTH))
        >= QR_FRAME_BORDER.mul_add(
            2.,
            LINK_TEXT_MIN_WIDTH + LINK_COLUMN_GAP + inline_qr_side(share),
        )
}

impl Desktop {
    pub(crate) fn share_copy_button(&self, id: usize, cx: &Context<Self>) -> Button {
        let copied =
            matches!(self.copy_feedback, CopyFeedback::Copied { share_id, .. } if share_id == id);
        Self::button(
            ("copy-share-link", id),
            language(cx).text(if copied { Text::Copied } else { Text::CopyLink }),
            if copied { Icon::Check } else { Icon::Copy },
            ButtonStyle::Primary,
            Action::CopyLink(id),
            cx,
        )
        .large()
        .full_width()
        .disabled(
            !self
                .shares
                .iter()
                .any(|share| share.id == id && share.usable_link().is_some()),
        )
    }

    pub(crate) fn share_link_disclosure(
        &self,
        theme: Theme,
        share: &Share,
        cx: &Context<Self>,
    ) -> Div {
        let Some(link) = share.usable_link() else {
            return column();
        };
        let locale = language(cx);
        let expanded = self.link_disclosure.expanded(share.id);
        let label = locale.text(if expanded {
            Text::HideFullLink
        } else {
            Text::ShowFullLink
        });
        column()
            .w_full()
            .min_w_0()
            .gap(px(8.))
            .child(
                Self::button(
                    ("share-link-disclosure", share.id),
                    label,
                    if expanded {
                        Icon::ChevronUp
                    } else {
                        Icon::ChevronDown
                    },
                    ButtonStyle::Quiet,
                    Action::ToggleLink(share.id),
                    cx,
                )
                .compact()
                .full_width(),
            )
            .when(expanded, |el| {
                el.child(
                    theme
                        .label(link.url(), 14.)
                        .font_family(FONT)
                        .w_full()
                        .min_w_0()
                        .p(px(12.))
                        .line_height(px(22.))
                        .rounded(px(10.))
                        .bg(theme.inset()),
                )
            })
    }

    pub(crate) fn share_qr(
        theme: Theme,
        share: Option<&Share>,
        enabled: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let side = inline_qr_side(share);
        let Some(share) = share.filter(|share| share.usable_link().is_some()) else {
            return column()
                .items_center()
                .gap(px(8.))
                .flex_shrink_0()
                .child(
                    column()
                        .size(px(QR_FRAME_BORDER.mul_add(2., side)))
                        .items_center()
                        .justify_center()
                        .rounded(px(12.))
                        .bg(theme.inset())
                        .child(icon(Icon::Link, 26.).text_color(theme.muted_text())),
                )
                .child(theme.caption(locale.text(Text::ScanToReceive)).opacity(0.4));
        };
        let Some(code) = share.usable_link().and_then(|link| link.qr().ok()) else {
            return column()
                .size(px(side))
                .flex_shrink_0()
                .items_center()
                .justify_center()
                .gap(px(8.))
                .child(icon(Icon::Link, 24.).text_color(theme.muted_text()))
                .child(theme.caption(locale.text(Text::QrUnavailable)));
        };
        column().flex_shrink_0().child(
            link_action(
                ("share-qr", share.id),
                locale.text(Text::EnlargeQr),
                theme,
                enabled,
                Action::EnlargeQr(share.id),
                cx,
            )
            .flex_col()
            .gap(px(8.))
            .child(
                div()
                    .border_1()
                    .border_color(theme.border())
                    .rounded(px(12.))
                    .overflow_hidden()
                    .child(code.view(side)),
            )
            .child(
                row()
                    .gap(px(7.))
                    .py(px(6.))
                    .child(locale.text(Text::ScanToReceive))
                    .child(icon(Icon::Expand, 14.).text_color(theme.muted_text())),
            ),
        )
    }

    pub(crate) fn toggle_link(&mut self, id: usize) {
        if self.shares.iter().any(|share| share.id == id) {
            self.link_disclosure.toggle(id);
            tracing::info!(
                event = "share.link_disclosure",
                session = std::process::id(),
                share_id = id,
                expanded = self.link_disclosure.expanded(id),
            );
        }
    }

    pub(crate) fn enlarge_qr(&mut self, id: usize, window: &mut Window, cx: &mut Context<Self>) {
        let Some(code) = self
            .shares
            .iter()
            .find(|share| share.id == id)
            .and_then(Share::usable_link)
            .and_then(|link| link.qr().ok())
        else {
            return;
        };
        tracing::info!(
            event = "share.qr_opened",
            session = std::process::id(),
            share_id = id,
            modules = code.modules_with_margin(),
        );
        self.open_overlay(Overlay::ShareQr(id), window, cx);
    }
}

fn link_action(
    id: impl Into<gpui::ElementId>,
    label: &'static str,
    theme: Theme,
    enabled: bool,
    action: Action,
    cx: &Context<Desktop>,
) -> Stateful<Div> {
    row()
        .id(id)
        .role(Role::Button)
        .aria_label(label)
        .rounded(px(10.))
        .border_1()
        .border_color(gpui::transparent_black())
        .text_size(px(13.))
        .text_color(theme.muted_text())
        .when(enabled, |el| {
            el.focusable()
                .tab_index(0)
                .cursor_pointer()
                .hover(|s| s.text_color(theme.accent_text()))
                .focus_visible(|s| s.border_color(theme.focus_ring()))
                .on_click(cx.listener(move |this, _, window, cx| {
                    cx.stop_propagation();
                    this.dispatch(action, window, cx);
                }))
        })
}
