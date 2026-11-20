use super::{LinkKind, ShareLink, SharePublication, qr_slot::QR_FRAME_SIDE};
use crate::{
    desktop::Desktop,
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
    /// `link` is the link this surface presents, which a result card still
    /// arriving withholds even when the share already has one.
    pub(crate) fn share_copy_button(
        &self,
        id: usize,
        link: Option<&ShareLink>,
        cx: &Context<Self>,
    ) -> Button {
        let copied = self.copy_feedback.matches(id, LinkKind::Standard);
        Self::button(
            ("copy-share-link", id),
            language(cx).text(if copied { Text::Copied } else { Text::CopyLink }),
            if copied { Icon::Check } else { Icon::Copy },
            ButtonStyle::Primary,
            Action::CopyLink(id, LinkKind::Standard),
            cx,
        )
        .large()
        .full_width()
        .disabled(link.is_none())
    }

    pub(crate) fn share_link_disclosure(
        &self,
        theme: Theme,
        id: usize,
        publication: Option<&SharePublication>,
        cx: &Context<Self>,
    ) -> Div {
        let Some(publication) = publication else {
            return column();
        };
        let link = publication.standard();
        let has_backup = publication.has_backup();
        let locale = language(cx);
        let expanded = self.link_disclosure.expanded(id);
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
                    ("share-link-disclosure", id),
                    label,
                    if expanded {
                        Icon::ChevronUp
                    } else {
                        Icon::ChevronDown
                    },
                    ButtonStyle::Quiet,
                    Action::ToggleLink(id),
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
                .when(has_backup, |el| {
                    let copied = self.copy_feedback.matches(id, LinkKind::Backup);
                    el.child(
                        Self::button(
                            ("copy-backup-link", id),
                            locale.text(if copied {
                                Text::Copied
                            } else {
                                Text::CopyBackupLink
                            }),
                            if copied { Icon::Check } else { Icon::Copy },
                            ButtonStyle::Secondary,
                            Action::CopyLink(id, LinkKind::Backup),
                            cx,
                        )
                        .compact()
                        .full_width(),
                    )
                })
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
