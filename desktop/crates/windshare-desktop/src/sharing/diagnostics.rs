use super::LinkKind;
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::Action,
};
use gpui::{Div, prelude::*, px};
use windshare_ui::{Button, ButtonStyle, Icon, Theme, Tone, column, row};

const DIAGNOSTIC_QR_SIDE: f32 = 224.;
const DIAGNOSTIC_GAP: f32 = 24.;

impl Desktop {
    pub(super) fn share_diagnostics_button(id: usize, cx: &Context<Self>) -> Button {
        Self::button(
            ("share-diagnostics", id),
            language(cx).text(Text::Diagnostics),
            Icon::Activity,
            ButtonStyle::Quiet,
            Action::OpenShareDiagnostics(id),
            cx,
        )
        .compact()
    }

    pub(crate) fn share_diagnostics(&self, theme: Theme, id: usize, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let Some(link) = self
            .shares
            .iter()
            .find(|share| share.id == id)
            .and_then(|share| share.link_for(LinkKind::Diagnostic))
        else {
            return theme.caption(locale.text(Text::DiagnosticLinkUnavailable));
        };
        let copied = self.copy_feedback.matches(id, LinkKind::Diagnostic);
        let controls = column()
            .flex_1()
            .min_w_0()
            .gap(px(16.))
            .child(theme.icon_tile(Icon::Activity, Tone::Lavender, 40.))
            .child(
                column()
                    .gap(px(6.))
                    .child(theme.heading(locale.text(Text::BrowserTrace), 18.))
                    .child(
                        theme
                            .caption(locale.text(Text::BrowserTraceDetail))
                            .line_height(px(21.)),
                    ),
            )
            .child(Self::button(
                ("copy-diagnostic-link", id),
                locale.text(if copied {
                    Text::Copied
                } else {
                    Text::CopyDiagnosticLink
                }),
                if copied { Icon::Check } else { Icon::Copy },
                ButtonStyle::Tinted(Tone::Lavender),
                Action::CopyDiagnosticLink(id),
                cx,
            ));
        let qr = link.qr().map_or_else(
            |_| {
                column().w(px(DIAGNOSTIC_QR_SIDE)).child(
                    theme
                        .caption(locale.text(Text::QrUnavailable))
                        .text_center(),
                )
            },
            |code| {
                column()
                    .items_center()
                    .gap(px(8.))
                    .child(
                        column()
                            .border_1()
                            .border_color(theme.border())
                            .rounded(px(12.))
                            .overflow_hidden()
                            .child(code.view(DIAGNOSTIC_QR_SIDE)),
                    )
                    .child(theme.caption(locale.text(Text::ScanDiagnosticQr)))
            },
        );
        row()
            .gap(px(DIAGNOSTIC_GAP))
            .child(controls)
            .child(qr.flex_shrink_0())
    }
}
