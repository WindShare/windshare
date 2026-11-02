use crate::{
    desktop::Desktop,
    flow_state::{LinkFeedback, RECEIVE_SAVE_MS, RECEIVED_FILES, ReceiveFlow},
    layout::{FlowLayout, RECEIVE_STAGE_HEIGHT},
    model::PreviewAction,
    motion::{SpiritMood, reassurance, reveal, scaled_spirit},
    share_timing::lerp,
};
use gpui::{Animation, AnimationExt, Div, FontWeight, div, prelude::*, px, relative};
use std::time::Duration;
use windshare_ui::{ButtonStyle, Icon, Theme, Tone, column, icon, row};

fn link_feedback(theme: Theme, feedback: LinkFeedback) -> Div {
    let (message, color) = match feedback {
        LinkFeedback::None => ("粘贴后按 Enter，即可打开分享", theme.muted_text()),
        LinkFeedback::Invalid => (
            "链接似乎不完整，请重新粘贴，或使用下方示例。",
            Tone::Rose.text(theme),
        ),
    };
    div()
        .h(px(22.))
        .child(theme.label(message, 14.).text_color(color))
}

impl Desktop {
    pub fn receiving_page(&self, theme: Theme, layout: FlowLayout, cx: &Context<Self>) -> Div {
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
                    .child(reassurance(theme, Icon::Shield, "只有你和分享者能读取文件"))
                    .child(reassurance(theme, Icon::Download, "接收前可查看文件")),
            )
    }

    fn receive_link(&self, theme: Theme, feedback: LinkFeedback, cx: &Context<Self>) -> Div {
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
                            .gap(px(10.))
                            .flex_1()
                            .child(theme.badge("接收一份分享", Tone::Lavender).w(px(104.)))
                            .child(theme.heading("从一个链接开始", 27.))
                            .child(
                                theme
                                    .label("粘贴链接，先看看对方分享了什么。", 16.)
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
                            .label("分享链接", 16.)
                            .font_weight(FontWeight::SEMIBOLD),
                    )
                    .child(
                        row()
                            .gap(px(10.))
                            .child(div().flex_1().min_w_0().child(self.link_input.clone()))
                            .child(
                                Self::button(
                                    "paste-link",
                                    "粘贴",
                                    Icon::Copy,
                                    ButtonStyle::Secondary,
                                    PreviewAction::PasteLink,
                                    cx,
                                )
                                .large(),
                            ),
                    )
                    .child(link_feedback(theme, feedback))
                    .child(
                        Self::button(
                            "open-link",
                            "打开分享",
                            Icon::Arrow,
                            ButtonStyle::Primary,
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
                    .child(theme.caption("还没有链接？"))
                    .child(
                        Self::button(
                            "demo-link",
                            "试试示例分享",
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
            .child(theme.heading("正在打开分享", 26.))
            .child(
                theme
                    .label("正在和分享者建立安全连接。", 16.)
                    .text_color(theme.muted_text()),
            )
            .child(
                Self::button(
                    "cancel-open",
                    "取消",
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
                            .child(theme.heading("京都的周末", 24.))
                            .child(theme.caption("来自 Alex · 3 个文件 · 共 24 MB")),
                    )
                    .child(theme.badge("分享者在线", Tone::Mint)),
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
                    .child(theme.caption("保存到 下载 / 京都的周末")),
            )
            .child(
                Self::button(
                    "save-files",
                    "接收这 3 个文件",
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
                        "换一个分享链接",
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
            .child(theme.heading("文件正在向你靠近", 26.))
            .child(
                theme
                    .label("京都的周末 · 3 个文件", 16.)
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
                    .child(theme.caption("正在演示接收过程").text_center()),
            )
            .child(
                Self::button(
                    "cancel-receive",
                    "取消接收",
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
                    .child(theme.heading("接收完成", 26.))
                    .child(
                        theme
                            .label("京都的周末 · 3 个文件 · 24 MB", 16.)
                            .text_color(theme.muted_text()),
                    ),
                "receive-complete",
                self.reduced_motion(),
            ))
            .child(theme.badge("演示完成，未写入实际文件", Tone::Lavender))
            .child(
                Self::button(
                    "receive-again",
                    "接收另一个分享",
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
