use super::link_views::{LINK_COLUMN_GAP, LINK_PANEL_WIDTH, link_columns_fit};
use crate::{
    desktop::Desktop,
    flow_state::ShareRun,
    layout::FlowLayout,
    localization::{Language, Text, language},
    model::{Action, Share, ShareState},
    share_animation::ShareScene,
};
use gpui::{Div, div, prelude::*, px, relative};
use windshare_ui::{ButtonStyle, Icon, Theme, Tone, column, icon, row};

const RESULT_SECTION_GAP: f32 = 24.;
const RESULT_FILE_ICON_SIZE: f32 = 52.;
const RESULT_TITLE_SIZE: f32 = 26.;
const RESULT_LINK_HEIGHT: f32 = 32.;
const LINK_ACTIVITY_WIDTH: f32 = 220.;
const LINK_ACTIVITY_HEIGHT: f32 = 4.;
const LINK_ACTIVITY_SEGMENT: f32 = 0.3;
const SETTLED_CONTENT_VISIBILITY: f32 = 0.95;

impl Desktop {
    pub(super) fn result_share(
        &self,
        theme: Theme,
        run: ShareRun,
        scene: ShareScene,
        layout: FlowLayout,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let Some(share) = self
            .shares
            .iter()
            .find(|share| share.id == run.selection_id)
        else {
            return column();
        };
        let ready = share.usable_link().is_some();
        let columns = link_columns_fit(scene.geometry.width, layout.panel_padding(), Some(share));
        let (status, tone) = share.status();
        let information = column()
            .flex_1()
            .min_w_0()
            .gap(px(RESULT_SECTION_GAP))
            .child(
                row()
                    .gap(px(7.))
                    .child(
                        icon(if ready { Icon::Check } else { Icon::Link }, 16.)
                            .text_color(tone.text(theme)),
                    )
                    .child(
                        theme
                            .caption(locale.text(if ready { Text::LinkReady } else { status }))
                            .text_color(tone.text(theme)),
                    ),
            )
            .child(file_summary(theme, share, locale))
            .when(share.error.is_some() || share.relay_reconnecting, |el| {
                el.child(Self::share_notice(theme, share, cx))
            })
            .child(self.result_action(theme, share, scene, cx));
        let card = theme
            .card()
            .w_full()
            .max_w(px(LINK_PANEL_WIDTH))
            .flex_shrink_0()
            .p(px(layout.panel_padding()))
            .gap(px(RESULT_SECTION_GAP))
            .opacity(scene.content_visibility())
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
                            .child(Self::share_qr(theme, Some(share), ready, cx))
                            .when(ready, |el| el.opacity(scene.frame.result_visibility)),
                    ),
            )
            .child(self.result_footer(theme, share, cx));
        // The result's natural height owns scrolling; expanded links and stacked QR
        // layouts must not be clipped by the finite animation stage.
        column()
            .relative()
            .w_full()
            .min_h(px(scene.geometry.height))
            .flex_shrink_0()
            .items_center()
            .justify_center()
            .child(card)
            .when(scene.decorations_visible(), |el| {
                el.child(scene.scenery(self.reduced_motion()))
            })
            .when(
                ready && scene.content_visibility() < SETTLED_CONTENT_VISIBILITY,
                |el| el.child(self.early_link(share, cx)),
            )
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
                    .gap(px(7.))
                    .opacity(if share.finished() { 0. } else { 1. })
                    .child(icon(Icon::Monitor, 16.).text_color(theme.muted_text()))
                    .child(theme.caption(locale.text(Text::KeepOpenToReceive))),
            )
            .child(
                row()
                    .gap(px(8.))
                    .when(!self.runtime.is_preview() && !pending, |el| {
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
                            locale.text(if pending {
                                Text::Cancel
                            } else {
                                Text::NewShare
                            }),
                            if pending { Icon::Close } else { Icon::Plus },
                            ButtonStyle::Quiet,
                            Action::ResetShare,
                            cx,
                        )
                        .compact()
                        .when(pending, |button| button.focus_handle(&self.primary_focus)),
                    ),
            )
    }

    fn result_action(
        &self,
        theme: Theme,
        share: &Share,
        scene: ShareScene,
        cx: &Context<Self>,
    ) -> Div {
        let mut actions = column().w_full().min_w_0().gap(px(8.)).child(
            self.share_copy_button(share.id, cx)
                .focus_handle(&self.primary_focus),
        );
        if share.usable_link().is_some() {
            actions = actions
                .child(
                    self.share_link_disclosure(theme, share, true, cx)
                        .opacity(scene.frame.result_visibility),
                )
                .child(
                    Self::button(
                        "result-open",
                        language(cx).text(Text::OpenInBrowser),
                        Icon::External,
                        ButtonStyle::Quiet,
                        Action::OpenLink(share.id),
                        cx,
                    )
                    .compact(),
                );
        } else if matches!(share.state, ShareState::Preparing) {
            actions = actions.child(
                row()
                    .h(px(RESULT_LINK_HEIGHT))
                    .justify_center()
                    .child(link_activity(theme, scene)),
            );
        }
        actions
    }

    pub(super) fn early_link(&self, share: &Share, cx: &Context<Self>) -> Div {
        // Preserve the original flight and landing, but never make those clocks a
        // prerequisite for using a link returned by the engine.
        let copied = matches!(self.copy_feedback, crate::flow_state::CopyFeedback::Copied { share_id, .. } if share_id == share.id);
        row()
            .absolute()
            .bottom_0()
            .w_full()
            .justify_center()
            .gap(px(12.))
            .child(Self::button(
                ("early-copy", share.id),
                language(cx).text(if copied { Text::Copied } else { Text::CopyLink }),
                if copied { Icon::Check } else { Icon::Copy },
                ButtonStyle::Primary,
                Action::CopyLink(share.id),
                cx,
            ))
            .child(Self::button(
                ("early-open", share.id),
                language(cx).text(Text::OpenInBrowser),
                Icon::External,
                ButtonStyle::Secondary,
                Action::OpenLink(share.id),
                cx,
            ))
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
            .when(share.relay_reconnecting, |el| {
                el.child(
                    theme
                        .caption(language(cx).text(Text::RelayReconnecting))
                        .text_color(Tone::Peach.text(theme)),
                )
            })
    }
}

fn link_activity(theme: Theme, scene: ShareScene) -> Div {
    let offset =
        (1. + LINK_ACTIVITY_SEGMENT).mul_add(scene.frame.activity_phase, -LINK_ACTIVITY_SEGMENT);
    div()
        .relative()
        .w(px(LINK_ACTIVITY_WIDTH))
        .max_w_full()
        .h(px(LINK_ACTIVITY_HEIGHT))
        .rounded_full()
        .overflow_hidden()
        .bg(Tone::Mint.surface(theme))
        .opacity(1. - scene.frame.result_visibility)
        .child(
            div()
                .absolute()
                .left(relative(offset))
                .w(relative(LINK_ACTIVITY_SEGMENT))
                .h_full()
                .rounded_full()
                .bg(theme.accent_text()),
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
