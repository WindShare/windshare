use super::link_views::{LINK_COLUMN_GAP, LINK_PANEL_WIDTH, link_columns_fit};
use crate::{
    desktop::Desktop,
    flow_state::{SampleBundle, ShareRun},
    layout::FlowLayout,
    localization::{Language, Text, language},
    model::{PreviewAction, Share},
    share_animation::ShareScene,
};
use gpui::{Div, div, prelude::*, px, relative};
use windshare_ui::{Button, ButtonStyle, Icon, Theme, Tone, column, icon, row};

const RESULT_SECTION_GAP: f32 = 24.;
const RESULT_FILE_ICON_SIZE: f32 = 52.;
const RESULT_TITLE_SIZE: f32 = 26.;
const RESULT_LINK_HEIGHT: f32 = 32.;
const LINK_ACTIVITY_WIDTH: f32 = 220.;
const LINK_ACTIVITY_HEIGHT: f32 = 4.;
const LINK_ACTIVITY_SEGMENT: f32 = 0.3;
const CONTROL_VISIBILITY_THRESHOLD: f32 = 0.95;

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
        let share = run
            .share_id()
            .and_then(|id| self.shares.iter().find(|share| share.id == id));
        let pending = share.is_none();
        let enabled = share.is_some() && scene.content_visibility() > CONTROL_VISIBILITY_THRESHOLD;
        let columns = link_columns_fit(scene.geometry.width, layout.panel_padding(), share);
        let information = column()
            .flex_1()
            .min_w_0()
            .gap(px(RESULT_SECTION_GAP))
            .child(result_status(theme, pending, locale))
            .child(file_summary(theme, run.bundle, locale))
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
                            .child(Self::share_qr(theme, share, enabled, cx))
                            .when(share.is_some(), |el| {
                                el.opacity(scene.frame.result_visibility)
                            }),
                    ),
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
                            .opacity(scene.frame.result_visibility)
                            .child(icon(Icon::Monitor, 16.).text_color(theme.muted_text()))
                            .child(theme.caption(locale.text(Text::KeepOpenToReceive))),
                    )
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
                            PreviewAction::ResetShare,
                            cx,
                        )
                        .compact()
                        .when(pending, |button| button.focus_handle(&self.primary_focus)),
                    ),
            );
        // The result's natural height owns scrolling. Fixed animation-stage heights
        // otherwise clip stacked QR layouts and expanded, unbroken capability URLs.
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
    }

    fn result_action(
        &self,
        theme: Theme,
        share: Option<&Share>,
        scene: ShareScene,
        cx: &Context<Self>,
    ) -> Div {
        let enabled = share.is_some() && scene.content_visibility() > CONTROL_VISIBILITY_THRESHOLD;
        let control = share
            .map_or_else(
                || {
                    Button::new("pending-copy", language(cx).text(Text::CopyLink))
                        .icon(Icon::Copy)
                        .style(ButtonStyle::Primary)
                        .large()
                        .full_width()
                },
                |share| self.share_copy_button(share.id, cx),
            )
            .focus_handle(&self.primary_focus)
            .disabled(!enabled);
        let mut actions = column().w_full().min_w_0().gap(px(8.)).child(control);
        if let Some(share) = share {
            actions = actions.child(
                self.share_link_disclosure(theme, share, enabled, cx)
                    .opacity(scene.frame.result_visibility),
            );
        } else {
            actions = actions.child(
                row()
                    .h(px(RESULT_LINK_HEIGHT))
                    .justify_center()
                    .child(link_activity(theme, scene)),
            );
        }
        actions
    }
}

fn result_status(theme: Theme, pending: bool, locale: Language) -> Div {
    row()
        .gap(px(7.))
        .child(
            icon(if pending { Icon::Link } else { Icon::Check }, 16.)
                .text_color(theme.accent_text()),
        )
        .child(
            theme
                .caption(locale.text(if pending {
                    Text::PreparingLink
                } else {
                    Text::LinkReady
                }))
                .text_color(theme.accent_text()),
        )
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

fn file_summary(theme: Theme, bundle: SampleBundle, locale: Language) -> Div {
    row()
        .gap(px(16.))
        .child(theme.icon_tile(bundle.icon(), Tone::Mint, RESULT_FILE_ICON_SIZE))
        .child(
            column()
                .flex_1()
                .min_w_0()
                .gap(px(6.))
                .child(
                    theme
                        .heading(bundle.name(), RESULT_TITLE_SIZE)
                        .line_clamp(2),
                )
                .child(theme.caption(format!(
                    "{} · {}",
                    bundle.contents().description(locale),
                    bundle.size()
                ))),
        )
}
