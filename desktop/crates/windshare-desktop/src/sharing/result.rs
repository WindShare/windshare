use super::{
    link_views::{LINK_COLUMN_GAP, LINK_PANEL_WIDTH, link_columns_fit},
    stage_height,
};
use crate::{
    desktop::Desktop,
    layout::{FlowLayout, task_width},
    localization::{Language, Text, language},
    model::{Action, Share, ShareState},
};
use gpui::{Div, Window, div, prelude::*, px};
use windshare_ui::{ButtonStyle, COMPACT_BUTTON_HEIGHT, Icon, Theme, Tone, column, icon, row};

const RESULT_SECTION_GAP: f32 = 24.;
const RESULT_FILE_ICON_SIZE: f32 = 52.;
const RESULT_TITLE_SIZE: f32 = 26.;

impl Desktop {
    pub(super) fn share_result(
        &self,
        theme: Theme,
        share: &Share,
        layout: FlowLayout,
        window: &Window,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let columns = link_columns_fit(task_width(window), layout.panel_padding());
        let information = column()
            .flex_1()
            .min_w_0()
            .gap(px(RESULT_SECTION_GAP))
            .child(result_status(theme, share, locale))
            .child(file_summary(theme, share, locale))
            .when(
                share.error.is_some() || share.relay_notice().is_some(),
                |el| el.child(Self::share_notice(theme, share, cx)),
            )
            .child(self.result_action(theme, share, cx));
        let card = theme
            .card()
            .w_full()
            .max_w(px(LINK_PANEL_WIDTH))
            .flex_shrink_0()
            .p(px(layout.panel_padding()))
            .gap(px(RESULT_SECTION_GAP))
            .child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(LINK_COLUMN_GAP))
                    .when(!columns, |el| el.flex_col().items_stretch())
                    .child(information)
                    .child(
                        column()
                            .items_center()
                            .flex_shrink_0()
                            .child(Self::share_qr(theme, share, cx)),
                    ),
            )
            .child(self.result_footer(theme, share, cx));
        // The result's natural height owns scrolling, including expanded links
        // and the stacked QR layout in a narrow window.
        column()
            .w_full()
            .min_h(px(stage_height(layout)))
            .flex_shrink_0()
            .items_center()
            .justify_center()
            .child(card)
    }

    fn result_footer(&self, theme: Theme, share: &Share, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let pending = matches!(share.state, ShareState::Preparing);
        row()
            .justify_between()
            .flex_wrap()
            .gap(px(8.))
            .pt(px(16.))
            .border_t_1()
            .border_color(theme.border())
            .child(
                row()
                    .min_h(px(COMPACT_BUTTON_HEIGHT))
                    .gap(px(7.))
                    .opacity(if share.finished() { 0. } else { 1. })
                    .child(icon(Icon::Monitor, 16.).text_color(theme.muted_text()))
                    .child(theme.caption(locale.text(Text::KeepOpenToReceive))),
            )
            .when(!pending, |el| {
                el.child(
                    row()
                        .gap(px(8.))
                        .when(!self.runtime.is_preview(), |el| {
                            let (text, symbol, action) = share.control();
                            el.child(
                                Self::button(
                                    "result-control",
                                    locale.text(text),
                                    symbol,
                                    ButtonStyle::Quiet,
                                    action,
                                    cx,
                                )
                                .compact()
                                .disabled(matches!(share.state, ShareState::Stopping)),
                            )
                        })
                        .child(
                            Self::button(
                                "share-again",
                                locale.text(Text::NewShare),
                                Icon::Plus,
                                ButtonStyle::Secondary,
                                Action::ResetShare,
                                cx,
                            )
                            .compact(),
                        ),
                )
            })
    }

    fn result_action(&self, theme: Theme, share: &Share, cx: &Context<Self>) -> Div {
        column()
            .w_full()
            .min_w_0()
            .gap(px(8.))
            .child(
                self.share_copy_button(share.id, cx)
                    .focus_handle(&self.primary_focus),
            )
            .child(
                column()
                    .min_h(px(COMPACT_BUTTON_HEIGHT))
                    .when(share.usable_link().is_some(), |el| {
                        el.child(self.share_link_disclosure(theme, share, cx))
                    }),
            )
    }

    pub(crate) fn share_notice(theme: Theme, share: &Share, cx: &Context<Self>) -> Div {
        column()
            .gap(px(8.))
            .when_some(share.error.as_ref(), |el, error| {
                el.child(
                    theme
                        .label(error.clone(), 15.)
                        .text_color(Tone::Rose.text(theme)),
                )
            })
            .when_some(share.relay_notice(), |el, message| {
                el.child(
                    theme
                        .caption(language(cx).text(message))
                        .text_color(Tone::Peach.text(theme)),
                )
            })
    }
}

fn result_status(theme: Theme, share: &Share, locale: Language) -> Div {
    let ready = share.usable_link().is_some();
    let (status, tone) = share.status();
    let color = if matches!(share.state, ShareState::Preparing) {
        theme.muted_text()
    } else {
        tone.text(theme)
    };
    row()
        .gap(px(7.))
        .child(icon(if ready { Icon::Check } else { Icon::Link }, 16.).text_color(color))
        .child(
            theme
                .caption(locale.text(if ready { Text::LinkReady } else { status }))
                .text_color(color),
        )
}

fn file_summary(theme: Theme, share: &Share, locale: Language) -> Div {
    row()
        .gap(px(16.))
        .child(theme.icon_tile(share.symbol, Tone::Mint, RESULT_FILE_ICON_SIZE))
        .child(
            column()
                .flex_1()
                .min_w_0()
                .gap(px(6.))
                .child(
                    theme
                        .heading(share.name.clone(), RESULT_TITLE_SIZE)
                        .line_clamp(2),
                )
                .child(theme.caption(share.description(locale))),
        )
}
