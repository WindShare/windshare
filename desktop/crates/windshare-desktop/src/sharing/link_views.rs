use super::{LinkKind, SharePublication, qr_slot::QR_FRAME_SIDE};
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::{Action, Overlay},
};
use gpui::{Div, Window, prelude::*, px};
use windshare_ui::{Button, ButtonStyle, FONT, Icon, Theme, Tone, column};

pub(super) const LINK_PANEL_WIDTH: f32 = 700.;
pub(super) const LINK_COLUMN_GAP: f32 = 32.;
pub(super) const LINK_TEXT_MIN_WIDTH: f32 = 280.;
pub(super) fn link_columns_fit(width: f32, padding: f32) -> bool {
    padding.mul_add(-2., width.min(LINK_PANEL_WIDTH))
        >= LINK_TEXT_MIN_WIDTH + LINK_COLUMN_GAP + QR_FRAME_SIDE + 2.
}
impl Desktop {
    /// `publication` is the link set this surface presents, which a result card still
    /// arriving withholds even when the share already has one.
    pub(crate) fn share_copy_button(
        &self,
        id: usize,
        publication: Option<&SharePublication>,
        cx: &Context<Self>,
    ) -> Button {
        let copied = self.copy_feedback.matches(id, LinkKind::Primary);
        let traced = publication.is_some_and(|pub_| pub_.is_traced(self.browser_trace));
        let label = if copied {
            Text::Copied
        } else if traced {
            Text::CopyDiagnosticLink
        } else {
            Text::CopyLink
        };
        let style = if traced {
            ButtonStyle::Tinted(Tone::Lavender)
        } else {
            ButtonStyle::Primary
        };
        Self::button(
            ("copy-share-link", id),
            language(cx).text(label),
            if copied { Icon::Check } else { Icon::Copy },
            style,
            Action::CopyLink(id, LinkKind::Primary),
            cx,
        )
        .large()
        .full_width()
        .disabled(publication.is_none())
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
        let link = publication.primary(self.browser_trace);
        let has_backup = publication.has_backup();
        let backup_traced = publication.is_backup_traced(self.browser_trace);
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
                    let backup_label = if copied {
                        Text::Copied
                    } else if backup_traced {
                        Text::CopyDiagnosticBackupLink
                    } else {
                        Text::CopyBackupLink
                    };
                    el.child(
                        Self::button(
                            ("copy-backup-link", id),
                            locale.text(backup_label),
                            if copied { Icon::Check } else { Icon::Copy },
                            if backup_traced {
                                ButtonStyle::Tinted(Tone::Lavender)
                            } else {
                                ButtonStyle::Secondary
                            },
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
        let Some(share) = self.shares.iter().find(|share| share.id == id) else {
            return;
        };
        let Some(code) = share
            .usable_link(self.browser_trace)
            .and_then(|link| link.qr().ok())
        else {
            return;
        };
        tracing::info!(
            event = "share.qr_opened",
            session = std::process::id(),
            share_id = id,
            browser_trace = share.is_traced(self.browser_trace),
            modules = code.modules_with_margin(),
        );
        self.open_overlay(Overlay::ShareQr(id), window, cx);
    }
}
