use super::qr::QR_FRAME_SIDE;
use crate::{
    desktop::Desktop,
    flow_state::CopyFeedback,
    localization::{Text, language},
    model::{Action, Overlay, Share},
};
use gpui::{Div, Window, prelude::*, px};
use windshare_ui::{Button, ButtonStyle, FONT, Icon, Theme, column};

pub(super) const LINK_PANEL_WIDTH: f32 = 700.;
pub(super) const LINK_COLUMN_GAP: f32 = 32.;
pub(super) const LINK_TEXT_MIN_WIDTH: f32 = 280.;
pub(super) fn link_columns_fit(width: f32, padding: f32) -> bool {
    padding.mul_add(-2., width.min(LINK_PANEL_WIDTH))
        >= LINK_TEXT_MIN_WIDTH + LINK_COLUMN_GAP + QR_FRAME_SIDE + 2.
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
