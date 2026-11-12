use super::link_views::{LINK_COLUMN_GAP, LINK_PANEL_WIDTH, link_columns_fit};
use crate::{
    desktop::Desktop,
    localization::{Language, Text, language},
    model::{Action, Share},
    overlays::{OVERLAY_MARGIN, OVERLAY_PADDING},
};
use gpui::{Div, Window, div, prelude::*, px};
use windshare_ui::{ButtonStyle, Icon, Theme, column, icon, row};

const QR_LARGE_MIN_SIDE: f32 = 352.;
const QR_LARGE_MODULE_SIDE: f32 = 6.;
const QR_DIALOG_CHROME_HEIGHT: f32 = 330.;

pub fn detail_width(window: &Window) -> f32 {
    LINK_PANEL_WIDTH.min(OVERLAY_MARGIN.mul_add(-2., f32::from(window.viewport_size().width)))
}

pub fn enlarged_qr_side(share: &Share, window: &Window) -> f32 {
    let desired =
        share
            .usable_link()
            .and_then(|link| link.qr().ok())
            .map_or(QR_LARGE_MIN_SIDE, |code| {
                code.side_for_modules(QR_LARGE_MODULE_SIDE)
                    .max(QR_LARGE_MIN_SIDE)
            });
    desired
        .min(
            (OVERLAY_MARGIN + OVERLAY_PADDING)
                .mul_add(-2., f32::from(window.viewport_size().width)),
        )
        .min(f32::from(window.viewport_size().height) - QR_DIALOG_CHROME_HEIGHT)
}

impl Desktop {
    pub(crate) fn share_detail(
        &self,
        theme: Theme,
        id: usize,
        window: &Window,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let Some(share) = self.shares.iter().find(|share| share.id == id) else {
            return column();
        };
        let information = column()
            .flex_1()
            .min_w_0()
            .gap(px(24.))
            .child(share_summary(theme, share, locale))
            .child(
                row()
                    .justify_between()
                    .flex_wrap()
                    .gap(px(8.))
                    .child(theme.caption(locale.created(&share.age.description(locale))))
                    .child(theme.badge(locale.text(share.status().0), share.status().1)),
            )
            .when(share.error.is_some() || share.relay_reconnecting, |el| {
                el.child(Self::share_notice(theme, share, cx))
            })
            .child(
                column()
                    .gap(px(8.))
                    .child(self.share_copy_button(id, cx))
                    .child(self.share_link_disclosure(theme, share, true, cx))
                    .when(share.usable_link().is_some(), |el| {
                        el.child(Self::button(
                            "detail-open",
                            locale.text(Text::OpenInBrowser),
                            Icon::External,
                            ButtonStyle::Quiet,
                            Action::OpenLink(id),
                            cx,
                        ))
                    }),
            );
        column()
            .gap(px(24.))
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(LINK_COLUMN_GAP))
                    .when(
                        !link_columns_fit(detail_width(window), OVERLAY_PADDING, Some(share)),
                        |el| el.flex_col().items_stretch(),
                    )
                    .child(information)
                    .child(column().items_center().child(Self::share_qr(
                        theme,
                        Some(share),
                        true,
                        cx,
                    ))),
            )
            .child(
                row()
                    .justify_between()
                    .flex_wrap()
                    .gap(px(8.))
                    .pt(px(16.))
                    .border_t_1()
                    .border_color(theme.border())
                    .child(
                        row()
                            .gap(px(7.))
                            .opacity(if share.finished() { 0. } else { 1. })
                            .child(icon(Icon::Monitor, 16.).text_color(theme.muted_text()))
                            .child(theme.caption(locale.text(Text::KeepOpenToReceive))),
                    )
                    .child(
                        Self::button(
                            "detail-toggle",
                            locale.text(share.control().0),
                            share.control().1,
                            ButtonStyle::Quiet,
                            share.control().2,
                            cx,
                        )
                        .compact()
                        .disabled(matches!(share.state, crate::model::ShareState::Stopping)),
                    ),
            )
    }

    pub(crate) fn share_qr_detail(
        &self,
        theme: Theme,
        id: usize,
        window: &Window,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let Some(share) = self.shares.iter().find(|share| share.id == id) else {
            return column();
        };
        column()
            .items_center()
            .gap(px(18.))
            .child(
                theme
                    .label(share.name.clone(), 17.)
                    .w_full()
                    .line_clamp(2)
                    .text_center(),
            )
            .child(
                share
                    .usable_link()
                    .and_then(|link| link.qr().ok())
                    .map_or_else(
                        || column().child(theme.caption(locale.text(Text::QrUnavailable))),
                        |code| column().child(code.view(enlarged_qr_side(share, window))),
                    ),
            )
            .child(
                row()
                    .gap(px(7.))
                    .child(icon(Icon::Monitor, 16.).text_color(theme.muted_text()))
                    .child(theme.caption(locale.text(Text::KeepOpenToReceive))),
            )
    }
}

fn share_summary(theme: Theme, share: &Share, locale: Language) -> Div {
    row()
        .gap(px(14.))
        .child(theme.icon_tile(share.symbol, share.tone, 48.))
        .child(
            column()
                .flex_1()
                .min_w_0()
                .gap(px(7.))
                .child(theme.heading(share.name.clone(), 22.).line_clamp(2))
                .child(theme.caption(share.description(locale))),
        )
}
