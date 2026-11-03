use crate::localization::{Language, Text, language};
use crate::{
    desktop::Desktop,
    flow_state::{LinkFeedback, RECEIVE_SAVE_MS, RECEIVED_FILES, ReceiveFlow, SampleBundle},
    layout::{FlowLayout, RECEIVE_STAGE_HEIGHT},
    model::PreviewAction,
    motion::{SpiritMood, reassurance, reveal, scaled_spirit},
    share_timing::lerp,
};
use gpui::{Animation, AnimationExt, Div, FontWeight, div, prelude::*, px, relative};
use std::time::Duration;
use windshare_ui::{ButtonStyle, Icon, Theme, Tone, column, icon, row};

fn link_feedback(theme: Theme, feedback: LinkFeedback, locale: Language) -> Option<Div> {
    let message = match feedback {
        LinkFeedback::None => return None,
        LinkFeedback::Invalid => locale.text(Text::InvalidLink),
    };
    Some(
        theme
            .label(message, 14.)
            .line_height(px(22.))
            .text_color(Tone::Rose.text(theme)),
    )
}

impl Desktop {
    pub fn receiving_page(&self, theme: Theme, layout: FlowLayout, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let content = match self.receive_flow {
            ReceiveFlow::Link { feedback } => self.receive_link(theme, feedback, cx),
            ReceiveFlow::Opening { .. } => self.opening_link(theme, layout, cx),
            ReceiveFlow::Files => self.receive_files(theme, cx),
            ReceiveFlow::Saving { operation } => self.receiving_files(theme, operation, layout, cx),
            ReceiveFlow::Saved => self.receive_complete(theme, layout, cx),
        };
        column()
            .gap(px(22.))
            .child(
                theme
                    .card()
                    .rounded(px(24.))
                    .p(px(layout.panel_padding()))
                    .child(
                        column()
                            .min_h(px(RECEIVE_STAGE_HEIGHT))
                            .justify_center()
                            .child(content),
                    ),
            )
            .child(
                row()
                    .justify_center()
                    .flex_wrap()
                    .gap(px(24.))
                    .child(reassurance(
                        theme,
                        Icon::Shield,
                        locale.text(Text::PrivateFiles),
                    ))
                    .child(reassurance(
                        theme,
                        Icon::Download,
                        locale.text(Text::InspectBeforeReceive),
                    )),
            )
    }

    fn receive_link(&self, theme: Theme, feedback: LinkFeedback, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        column()
            .gap(px(24.))
            .w_full()
            .max_w(px(700.))
            .mx_auto()
            .child(
                row()
                    .gap(px(20.))
                    .justify_between()
                    .child(
                        column()
                            .gap(px(12.))
                            .flex_1()
                            .child(
                                theme
                                    .heading(locale.text(Text::StartWithLink), 27.)
                                    .line_height(px(36.)),
                            )
                            .child(
                                theme
                                    .label(locale.text(Text::PasteLinkDetail), 16.)
                                    .line_height(px(26.))
                                    .text_color(theme.muted_text()),
                            ),
                    )
                    .child(scaled_spirit(
                        "receive-link-spirit",
                        SpiritMood::Idle,
                        Tone::Lavender,
                        0.58,
                        self.reduced_motion(),
                    )),
            )
            .child(
                column()
                    .gap(px(12.))
                    .child(
                        theme
                            .label(locale.text(Text::ShareLink), 16.)
                            .font_weight(FontWeight::SEMIBOLD),
                    )
                    .child(
                        row()
                            .gap(px(10.))
                            .child(div().flex_1().min_w_0().child(self.link_input.clone()))
                            .child(
                                Self::button(
                                    "paste-link",
                                    locale.text(Text::Paste),
                                    Icon::Copy,
                                    ButtonStyle::Secondary,
                                    PreviewAction::PasteLink,
                                    cx,
                                )
                                .large(),
                            ),
                    )
                    .children(link_feedback(theme, feedback, locale))
                    .child(
                        Self::button(
                            "open-link",
                            locale.text(Text::OpenShare),
                            Icon::Arrow,
                            ButtonStyle::Tinted(Tone::Lavender),
                            PreviewAction::OpenPreview,
                            cx,
                        )
                        .large()
                        .focus_handle(&self.primary_focus),
                    ),
            )
            .child(
                row()
                    .justify_between()
                    .gap(px(16.))
                    .pt(px(4.))
                    .child(theme.caption(locale.text(Text::NoLink)))
                    .child(
                        Self::button(
                            "demo-link",
                            locale.text(Text::TrySample),
                            Icon::Spark,
                            ButtonStyle::Ghost,
                            PreviewAction::DemoLink,
                            cx,
                        )
                        .compact(),
                    ),
            )
    }

    fn opening_link(&self, theme: Theme, layout: FlowLayout, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        column()
            .items_center()
            .gap(px(20.))
            .child(scaled_spirit(
                "receive-opening-spirit",
                SpiritMood::Receiving,
                Tone::Lavender,
                layout.scene_scale(),
                self.reduced_motion(),
            ))
            .child(theme.heading(locale.text(Text::OpeningShare), 26.))
            .child(
                theme
                    .label(locale.text(Text::ConnectingSecurely), 16.)
                    .text_color(theme.muted_text()),
            )
            .child(
                Self::button(
                    "cancel-open",
                    locale.text(Text::Cancel),
                    Icon::Close,
                    ButtonStyle::Ghost,
                    PreviewAction::ResetReceive,
                    cx,
                )
                .compact()
                .focus_handle(&self.primary_focus),
            )
    }

    fn receive_files(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        column()
            .gap(px(16.))
            .child(reveal(
                row()
                    .gap(px(16.))
                    .child(theme.icon_tile(Icon::Folder, Tone::Peach, 56.))
                    .child(
                        column()
                            .gap(px(6.))
                            .flex_1()
                            .child(theme.heading(SampleBundle::Photos.name(), 24.))
                            .child(theme.caption(locale.received_from(
                                "Alex",
                                &format!(
                                    "{} · {}",
                                    locale.files(RECEIVED_FILES.len()),
                                    SampleBundle::Photos.size()
                                ),
                            ))),
                    )
                    .child(theme.badge(locale.text(Text::SenderOnline), Tone::Mint)),
                "receive-files",
                self.reduced_motion(),
            ))
            .child(
                column().children(RECEIVED_FILES.into_iter().enumerate().map(
                    |(index, (name, size, symbol))| {
                        row()
                            .gap(px(14.))
                            .py(px(12.))
                            .border_t_1()
                            .border_color(theme.border())
                            .child(theme.icon_tile(
                                symbol,
                                if index == 2 {
                                    Tone::Lavender
                                } else {
                                    Tone::Peach
                                },
                                38.,
                            ))
                            .child(theme.label(name, 16.).flex_1())
                            .child(theme.caption(size))
                    },
                )),
            )
            .child(
                row()
                    .gap(px(8.))
                    .child(icon(Icon::Folder, 17.).text_color(theme.muted_text()))
                    .child(theme.caption(locale.save_to_downloads(SampleBundle::Photos.name()))),
            )
            .child(
                Self::button(
                    "save-files",
                    locale.receive_files(RECEIVED_FILES.len()),
                    Icon::Download,
                    ButtonStyle::Primary,
                    PreviewAction::SavePreview,
                    cx,
                )
                .large()
                .focus_handle(&self.primary_focus),
            )
            .child(
                row().justify_center().child(
                    Self::button(
                        "receive-other",
                        locale.text(Text::AnotherLink),
                        Icon::Link,
                        ButtonStyle::Ghost,
                        PreviewAction::ResetReceive,
                        cx,
                    )
                    .compact(),
                ),
            )
    }

    fn receiving_files(
        &self,
        theme: Theme,
        operation: usize,
        layout: FlowLayout,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let progress_bar = div()
            .h(px(7.))
            .w(relative(0.05))
            .rounded_full()
            .bg(Tone::Lavender.accent(theme));
        let progress_bar = if self.reduced_motion() {
            progress_bar.w(relative(0.65)).into_any_element()
        } else {
            progress_bar
                .with_animation(
                    ("receive-progress", operation),
                    Animation::new(Duration::from_millis(RECEIVE_SAVE_MS)),
                    |el, delta| el.w(relative(lerp(0.05, 1., delta))),
                )
                .into_any_element()
        };
        column()
            .items_center()
            .gap(px(18.))
            .child(scaled_spirit(
                "receive-saving-spirit",
                SpiritMood::Receiving,
                Tone::Lavender,
                layout.scene_scale(),
                self.reduced_motion(),
            ))
            .child(theme.heading(locale.text(Text::ReceivingFiles), 26.))
            .child(
                theme
                    .label(
                        format!(
                            "{} · {}",
                            SampleBundle::Photos.name(),
                            locale.files(RECEIVED_FILES.len())
                        ),
                        16.,
                    )
                    .text_color(theme.muted_text()),
            )
            .child(
                column()
                    .w_full()
                    .max_w(px(460.))
                    .gap(px(12.))
                    .child(
                        div()
                            .h(px(7.))
                            .w_full()
                            .rounded_full()
                            .bg(Tone::Lavender.surface(theme))
                            .overflow_hidden()
                            .child(progress_bar),
                    )
                    .child(
                        theme
                            .caption(locale.text(Text::ReceiveSimulation))
                            .text_center(),
                    ),
            )
            .child(
                Self::button(
                    "cancel-receive",
                    locale.text(Text::CancelReceive),
                    Icon::Close,
                    ButtonStyle::Ghost,
                    PreviewAction::ResetReceive,
                    cx,
                )
                .compact()
                .focus_handle(&self.primary_focus),
            )
    }

    fn receive_complete(&self, theme: Theme, layout: FlowLayout, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        column()
            .items_center()
            .gap(px(18.))
            .child(reveal(
                column()
                    .items_center()
                    .gap(px(16.))
                    .child(scaled_spirit(
                        "receive-complete-spirit",
                        SpiritMood::Happy,
                        Tone::Lavender,
                        layout.scene_scale(),
                        self.reduced_motion(),
                    ))
                    .child(theme.heading(locale.text(Text::ReceiveComplete), 26.))
                    .child(
                        theme
                            .label(
                                format!(
                                    "{} · {} · {}",
                                    SampleBundle::Photos.name(),
                                    locale.files(RECEIVED_FILES.len()),
                                    SampleBundle::Photos.size()
                                ),
                                16.,
                            )
                            .text_color(theme.muted_text()),
                    ),
                "receive-complete",
                self.reduced_motion(),
            ))
            .child(theme.badge(locale.text(Text::PreviewComplete), Tone::Lavender))
            .child(
                Self::button(
                    "receive-again",
                    locale.text(Text::ReceiveAnother),
                    Icon::Link,
                    ButtonStyle::Primary,
                    PreviewAction::ResetReceive,
                    cx,
                )
                .large()
                .focus_handle(&self.primary_focus),
            )
    }
}
