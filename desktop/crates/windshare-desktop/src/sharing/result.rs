use super::{
    SharePublication,
    intro::IntroFrame,
    link_views::{LINK_COLUMN_GAP, LINK_PANEL_WIDTH, link_columns_fit},
    qr_slot::{Arrival, QrSlot},
    stage_height,
};
use crate::{
    desktop::Desktop,
    layout::{FlowLayout, task_width},
    localization::{Language, Text, language},
    model::{Action, Share, ShareState},
    motion::ShareAnimation,
};
use gpui::{
    AnimationExt, Div, SpringAnimation, SpringConfig, SpringPlayback, Window, div, prelude::*, px,
};
use windshare_ui::{ButtonStyle, COMPACT_BUTTON_HEIGHT, Icon, Theme, Tone, column, icon, row};

const RESULT_SECTION_GAP: f32 = 24.;
const RESULT_FILE_ICON_SIZE: f32 = 52.;
const RESULT_TITLE_SIZE: f32 = 26.;
// Critical damping lets readiness arrive softly without overshooting opacity.
const READINESS_FADE: SpringConfig = SpringConfig::new(400., 40., 1.);

impl Desktop {
    pub(super) fn share_result(
        &self,
        theme: Theme,
        share: &Share,
        layout: FlowLayout,
        intro: IntroFrame,
        window: &Window,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let columns = link_columns_fit(task_width(window), layout.panel_padding());
        // Until its run is revealed the card says nothing newer than
        // "preparing", so the status, the copy action and the code all turn on
        // the beat the entrance finishes instead of trickling in under the flight.
        let revealed = intro.revealed();
        let publication = share.live_publication().filter(|_| revealed);
        let link = publication.map(|pub_| pub_.primary(self.browser_trace));
        let traced = publication.is_some_and(|pub_| pub_.is_traced(self.browser_trace));
        let pending = !revealed || matches!(share.state, ShareState::Preparing { .. });
        let playback = if intro.still || !window.is_visible() || !window.is_window_active() {
            SpringPlayback::Completed
        } else {
            SpringPlayback::Running
        };
        let settled = if link.is_some() {
            ResultStatus::Ready { traced }
        } else {
            ResultStatus::Other
        };
        let information = column()
            .flex_1()
            .min_w_0()
            .gap(px(RESULT_SECTION_GAP))
            .opacity(intro.information_presence())
            .child(result_status_reveal(
                theme, share, settled, pending, playback, locale,
            ))
            .child(file_summary(theme, share, locale))
            .when(
                revealed && (share.error().is_some() || share.relay_notice().is_some()),
                |el| el.child(Self::share_notice(theme, share, cx)),
            )
            .child(
                self.result_action(theme, share.id, publication, cx)
                    .with_spring(
                        ("result-action-reveal", share.id),
                        readiness_spring(link.is_some(), playback),
                        gpui::Styled::opacity,
                    ),
            );
        let slot = QrSlot::new(
            ("result-qr", share.id),
            link.and_then(|link| link.qr().ok()).cloned(),
            pending,
            locale.text(Text::QrUnavailable),
        )
        .arrival(match intro.run.animation {
            ShareAnimation::Whirl => Arrival::Whirl(Some(intro.run.wind())),
            ShareAnimation::Paper => Arrival::Fade,
        })
        .anchor(&self.intro_anchors.slot)
        .still(intro.still);
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
                            .child(Self::share_qr(theme, share.id, slot, traced, cx)),
                    ),
            )
            .child(self.result_footer(theme, share, pending, cx));
        // The result's natural height owns scrolling, including expanded links
        // and the stacked QR layout in a narrow window.
        column()
            .relative()
            .w_full()
            .min_h(px(stage_height(layout)))
            .flex_shrink_0()
            .items_center()
            .justify_center()
            .child(
                card.relative()
                    .top(px(intro.card_rise()))
                    .opacity(intro.card_presence()),
            )
            .children(self.intro_layers(theme, intro, layout, cx))
    }

    fn result_footer(&self, theme: Theme, share: &Share, pending: bool, cx: &Context<Self>) -> Div {
        let locale = language(cx);
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
                    .opacity(if !pending && share.finished() { 0. } else { 1. })
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

    fn result_action(
        &self,
        theme: Theme,
        id: usize,
        publication: Option<&SharePublication>,
        cx: &Context<Self>,
    ) -> Div {
        column()
            .w_full()
            .min_w_0()
            .gap(px(8.))
            .child(
                self.share_copy_button(id, publication, cx)
                    .focus_handle(&self.primary_focus),
            )
            .child(
                column()
                    .min_h(px(COMPACT_BUTTON_HEIGHT))
                    .child(self.share_link_disclosure(theme, id, publication, cx)),
            )
    }

    pub(crate) fn share_notice(theme: Theme, share: &Share, cx: &Context<Self>) -> Div {
        column()
            .gap(px(8.))
            .when_some(share.error(), |el, error| {
                el.child(
                    theme
                        .label(error.to_owned(), 15.)
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

fn readiness_spring(ready: bool, playback: SpringPlayback) -> SpringAnimation<f32> {
    SpringAnimation::new(READINESS_FADE)
        .to(if ready { 1. } else { 0. })
        .playback(playback)
}

fn result_status_reveal(
    theme: Theme,
    share: &Share,
    settled: ResultStatus,
    pending: bool,
    playback: SpringPlayback,
    locale: Language,
) -> impl IntoElement {
    let settled_status = result_status(theme, share, settled, locale);
    let preparing_status = result_status(theme, share, ResultStatus::Preparing, locale);
    column().relative().with_spring(
        ("result-status-reveal", share.id),
        readiness_spring(!pending, playback),
        move |el, presence| {
            // Only the changing status crossfades; the filename and its
            // geometry stay fixed, including when the link arrives late.
            el.child(settled_status.opacity(presence)).child(
                preparing_status
                    .absolute()
                    .top_0()
                    .left_0()
                    .opacity(1. - presence),
            )
        },
    )
}

#[derive(Clone, Copy)]
enum ResultStatus {
    Preparing,
    Ready { traced: bool },
    Other,
}

fn result_status(theme: Theme, share: &Share, state: ResultStatus, locale: Language) -> Div {
    let (status, tone) = share.status();
    let (symbol, text, color, traced) = match state {
        ResultStatus::Ready { traced } => (Icon::Check, Text::LinkReady, tone.text(theme), traced),
        ResultStatus::Preparing => (Icon::Link, Text::PreparingLink, theme.muted_text(), false),
        ResultStatus::Other => (Icon::Link, status, tone.text(theme), false),
    };
    let trace_color = Tone::Lavender.text(theme);
    row()
        .gap(px(7.))
        .child(icon(symbol, 16.).text_color(color))
        .child(theme.caption(locale.text(text)).text_color(color))
        .when(traced, |el| {
            el.child(theme.caption("·"))
                .child(icon(Icon::Activity, 14.).text_color(trace_color))
                .child(
                    theme
                        .caption(locale.text(Text::BrowserTrace))
                        .text_color(trace_color),
                )
        })
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
