use super::link_views::{LINK_COLUMN_GAP, LINK_PANEL_WIDTH, link_columns_fit};
use crate::{
    desktop::Desktop,
    localization::{Language, Text, language},
    model::{PreviewAction, Share},
    overlays::{OVERLAY_MARGIN, OVERLAY_PADDING},
};
use gpui::{Div, Window, div, prelude::*, px};
use windshare_ui::{ButtonStyle, Icon, Theme, Tone, column, icon, row};

const QR_LARGE_MIN_SIDE: f32 = 352.;
const QR_LARGE_MODULE_SIDE: f32 = 6.;
const QR_DIALOG_CHROME_HEIGHT: f32 = 330.;

pub fn detail_width(window: &Window) -> f32 {
    LINK_PANEL_WIDTH.min(OVERLAY_MARGIN.mul_add(-2., f32::from(window.viewport_size().width)))
}

pub fn enlarged_qr_side(share: &Share, window: &Window) -> f32 {
    let desired = share.link.qr().map_or(QR_LARGE_MIN_SIDE, |code| {
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
                    .child(theme.badge(
                        locale.text(if share.live() {
                            Text::SharingActive
                        } else {
                            Text::Paused
                        }),
                        if share.live() {
                            Tone::Mint
                        } else {
                            Tone::Neutral
                        },
                    )),
            )
            .child(
                column()
                    .gap(px(8.))
                    .child(self.share_copy_button(id, cx))
                    .child(self.share_link_disclosure(theme, share, true, cx)),
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
                            .child(icon(Icon::Monitor, 16.).text_color(theme.muted_text()))
                            .child(theme.caption(locale.text(Text::KeepOpenToReceive))),
                    )
                    .child(
                        Self::button(
                            "detail-toggle",
                            locale.text(if share.live() {
                                Text::PauseShare
                            } else {
                                Text::ResumeShare
                            }),
                            if share.live() {
                                Icon::Pause
                            } else {
                                Icon::Play
                            },
                            ButtonStyle::Quiet,
                            PreviewAction::ToggleShare(id),
                            cx,
                        )
                        .compact(),
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
            .child(share.link.qr().map_or_else(
                |_| column().child(theme.caption(locale.text(Text::QrUnavailable))),
                |code| column().child(code.view(enlarged_qr_side(share, window))),
            ))
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
                .child(theme.caption(format!(
                    "{} \u{b7} {}",
                    share.contents.description(locale),
                    share.size
                ))),
        )
}
