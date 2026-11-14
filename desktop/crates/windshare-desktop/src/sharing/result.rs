use super::{
    activity::link_activity,
    link_views::{LINK_COLUMN_GAP, LINK_PANEL_WIDTH, link_columns_fit},
    preparation_message,
};
use crate::{
    desktop::Desktop,
    flow_state::ShareRun,
    layout::FlowLayout,
    localization::{Language, Text, language},
    model::{Action, Share, ShareState},
    share_animation::ShareScene,
    share_timing::{LinkPresentation, lerp},
};
use gpui::{Div, div, prelude::*, px, relative};
use windshare_ui::{
    ButtonStyle, COMPACT_BUTTON_HEIGHT, Icon, Theme, Tone, column, icon, reserve_layout, row,
};

const RESULT_SECTION_GAP: f32 = 24.;
const RESULT_FILE_ICON_SIZE: f32 = 52.;
const RESULT_TITLE_SIZE: f32 = 26.;

impl Desktop {
    pub(super) fn running_share(
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
        let columns = link_columns_fit(scene.geometry.width, layout.panel_padding());

        let information = column()
            .flex_1()
            .min_w_0()
            .gap(px(RESULT_SECTION_GAP))
            .child(result_status(theme, share, scene.frame.link, locale))
            .child(file_summary(theme, share, locale))
            .when(
                share.error.is_some() || share.relay_notice().is_some(),
                |el| el.child(Self::share_notice(theme, share, cx)),
            )
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
                            .child(Self::share_qr(theme, share, scene.frame.link, cx)),
                    ),
            )
            .child(self.result_footer(theme, share, scene.frame.link, cx));
        // The result's natural height owns scrolling; expanded links and stacked QR
        // layouts must not be clipped by the finite animation stage.
        column()
            .relative()
            .w_full()
            .min_h(px(scene.geometry.height))
            .flex_shrink_0()
            .items_center()
            .justify_center()
            .child(if scene.content_visibility() > 0. {
                card.into_any_element()
            } else {
                reserve_layout(card).into_any_element()
            })
            .when(scene.decorations_visible(), |el| {
                el.child(
                    // The choreography keeps one centered coordinate space even when
                    // the result needs more height for a QR code or a wrapped filename.
                    div()
                        .id(("share-choreography", run.operation))
                        .absolute()
                        .left_0()
                        .top(relative(0.5))
                        .mt(px(-scene.geometry.height / 2.))
                        .w_full()
                        .h(px(scene.geometry.height))
                        .child(scene.scenery(self.reduced_motion()))
                        .child(preparation_message(
                            theme,
                            &share.name,
                            scene.preparation_visibility(),
                            locale,
                            ready,
                        )),
                )
            })
    }

    fn result_footer(
        &self,
        theme: Theme,
        share: &Share,
        presentation: LinkPresentation,
        cx: &Context<Self>,
    ) -> Div {
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
                    // Keep the card settled when result controls become available.
                    .min_h(px(COMPACT_BUTTON_HEIGHT))
                    .gap(px(7.))
                    .opacity(if share.finished() { 0. } else { 1. })
                    .child(icon(Icon::Monitor, 16.).text_color(theme.muted_text()))
                    .child(theme.caption(locale.text(Text::KeepOpenToReceive))),
            )
            .when(!pending, |el| {
                el.child(
                    row()
                        .opacity(presentation.details_visibility())
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
        share: &Share,
        scene: ShareScene,
        cx: &Context<Self>,
    ) -> Div {
        let ready = share.usable_link().is_some();
        let visibility = scene.frame.link.content_visibility();
        column()
            .w_full()
            .min_w_0()
            .gap(px(8.))
            .child(
                div()
                    .opacity(if ready { lerp(0.4, 1., visibility) } else { 1. })
                    .child(
                        self.share_copy_button(share.id, cx)
                            .focus_handle(&self.primary_focus),
                    ),
            )
            .child(
                column()
                    .relative()
                    .min_h(px(COMPACT_BUTTON_HEIGHT))
                    .when(ready, |el| {
                        el.child(
                            self.share_link_disclosure(theme, share, cx)
                                .opacity(scene.frame.link.details_visibility()),
                        )
                    })
                    .when(
                        (matches!(share.state, ShareState::Preparing) || ready)
                            && scene.frame.link.waiting_visibility() > 0.,
                        |el| {
                            el.child(
                                row()
                                    .absolute()
                                    .top_0()
                                    .left_0()
                                    .w_full()
                                    .h(px(COMPACT_BUTTON_HEIGHT))
                                    .justify_center()
                                    .child(link_activity(theme, scene.frame.link)),
                            )
                        },
                    ),
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

fn result_status(
    theme: Theme,
    share: &Share,
    presentation: LinkPresentation,
    locale: Language,
) -> Div {
    let ready = share.usable_link().is_some();
    let (status, tone) = share.status();
    let pending = matches!(share.state, ShareState::Preparing);
    let visibility = presentation.content_visibility();
    let label = |symbol, text, color| {
        row()
            .gap(px(7.))
            .child(icon(symbol, 16.).text_color(color))
            .child(theme.caption(locale.text(text)).text_color(color))
    };
    div()
        .relative()
        .child(
            label(
                if ready { Icon::Check } else { Icon::Link },
                if ready { Text::LinkReady } else { status },
                if pending {
                    theme.muted_text()
                } else {
                    tone.text(theme)
                },
            )
            .opacity(if ready { visibility } else { 1. }),
        )
        .when(ready && visibility < 1., |el| {
            el.child(
                label(Icon::Link, Text::PreparingLink, theme.muted_text())
                    .absolute()
                    .inset_0()
                    .opacity(1. - visibility),
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
