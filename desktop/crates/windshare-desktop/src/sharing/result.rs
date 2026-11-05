use crate::{
    desktop::Desktop,
    flow_state::{CopyFeedback, SampleBundle, ShareRun},
    layout::FlowLayout,
    localization::{Language, Text, language},
    model::{PreviewAction, Share},
    share_animation::ShareScene,
};
use gpui::{Div, div, prelude::*, px, relative};
use windshare_ui::{Button, ButtonStyle, FONT, Icon, Theme, Tone, column, icon, row};

const RESULT_WIDTH: f32 = 520.;
const RESULT_SECTION_GAP: f32 = 24.;
const RESULT_FILE_ICON_SIZE: f32 = 52.;
const RESULT_TITLE_SIZE: f32 = 26.;
const RESULT_LINK_HEIGHT: f32 = 24.;
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
        let card = theme
            .card()
            .w_full()
            .max_w(px(RESULT_WIDTH))
            .p(px(layout.panel_padding()))
            .gap(px(RESULT_SECTION_GAP))
            .opacity(scene.content_visibility())
            .child(
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
                    ),
            )
            .child(file_summary(theme, run.bundle, locale))
            .child(self.result_action(theme, share, scene, cx))
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
        // Only artwork uses stage coordinates; content keeps its own natural
        // layout so localization and window changes cannot split the result apart.
        column()
            .relative()
            .w_full()
            .h(px(scene.geometry.height))
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
        let locale = language(cx);
        let copied = share.is_some_and(|share| {
            matches!(self.copy_feedback, CopyFeedback::Copied { share_id, .. } if share_id == share.id)
        });
        let enabled = share.is_some() && scene.content_visibility() > CONTROL_VISIBILITY_THRESHOLD;
        let control = Button::new(
            "ready-copy",
            locale.text(if copied { Text::Copied } else { Text::CopyLink }),
        )
        .icon(if copied { Icon::Check } else { Icon::Copy })
        .style(ButtonStyle::Primary)
        .large()
        .full_width()
        .focus_handle(&self.primary_focus)
        .disabled(!enabled);
        let control = if let Some(share) = share {
            let id = share.id;
            control.on_press(cx.listener(move |this, (), window, cx| {
                this.dispatch(PreviewAction::CopyLink(id), window, cx);
            }))
        } else {
            control
        };
        column().gap(px(10.)).child(control).child(
            row()
                .relative()
                .w_full()
                .h(px(RESULT_LINK_HEIGHT))
                .justify_center()
                .when(scene.frame.result_visibility < 1., |el| {
                    el.child(link_activity(theme, scene))
                })
                .when_some(share, |el, share| {
                    el.child(
                        theme
                            .caption(share.link())
                            .absolute()
                            .inset_0()
                            .text_center()
                            .font_family(FONT)
                            .truncate()
                            .opacity(scene.frame.result_visibility),
                    )
                }),
        )
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

fn file_summary(theme: Theme, bundle: SampleBundle, locale: Language) -> Div {
    row()
        .gap(px(16.))
        .child(theme.icon_tile(bundle.icon(), Tone::Mint, RESULT_FILE_ICON_SIZE))
        .child(
            column()
                .flex_1()
                .min_w_0()
                .gap(px(6.))
                .child(theme.heading(bundle.name(), RESULT_TITLE_SIZE).truncate())
                .child(theme.caption(format!(
                    "{} · {}",
                    bundle.contents().description(locale),
                    bundle.size()
                ))),
        )
}
