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
use windshare_ui::{
    ButtonStyle, Icon, Tone, badge, border, caption, card, column, heading, icon, icon_tile, label,
    muted, row,
};

impl Desktop {
    pub fn receiving_page(&self, layout: FlowLayout, cx: &Context<Self>) -> Div {
        let content = match self.receive_flow {
            ReceiveFlow::Link { feedback } => self.receive_link(feedback, cx),
            ReceiveFlow::Opening { .. } => self.opening_link(layout, cx),
            ReceiveFlow::Files => self.receive_files(cx),
            ReceiveFlow::Saving { operation } => self.receiving_files(operation, layout, cx),
            ReceiveFlow::Saved => self.receive_complete(layout, cx),
        };
        column()
            .gap(px(22.))
            .child(
                card().rounded(px(24.)).p(px(layout.panel_padding())).child(
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
                    .child(reassurance(Icon::Shield, "只有你和分享者能读取文件"))
                    .child(reassurance(Icon::Download, "接收前可查看文件")),
            )
    }

    fn receive_link(&self, feedback: LinkFeedback, cx: &Context<Self>) -> Div {
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
                            .child(badge("接收一份分享", Tone::Lavender).w(px(104.)))
                            .child(heading("从一个链接开始", 27.))
                            .child(
                                label("粘贴链接，先看看对方分享了什么。", 16.).text_color(muted()),
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
                    .child(label("分享链接", 16.).font_weight(FontWeight::SEMIBOLD))
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
                    .child(
                        div().h(px(22.)).child(
                            label(
                                if matches!(feedback, LinkFeedback::Invalid) {
                                    "链接似乎不完整，请重新粘贴，或使用下方示例。"
                                } else {
                                    "粘贴后按 Enter，即可打开分享"
                                },
                                14.,
                            )
                            .text_color(
                                if matches!(feedback, LinkFeedback::Invalid) {
                                    Tone::Rose.text()
                                } else {
                                    muted()
                                },
                            ),
                        ),
                    )
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
                    .child(caption("还没有链接？"))
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

    fn opening_link(&self, layout: FlowLayout, cx: &Context<Self>) -> Div {
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
            .child(heading("正在打开分享", 26.))
            .child(label("正在和分享者建立安全连接。", 16.).text_color(muted()))
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

    fn receive_files(&self, cx: &Context<Self>) -> Div {
        column()
            .gap(px(16.))
            .child(reveal(
                row()
                    .gap(px(16.))
                    .child(icon_tile(Icon::Folder, Tone::Peach, 56.))
                    .child(
                        column()
                            .gap(px(6.))
                            .flex_1()
                            .child(heading("京都的周末", 24.))
                            .child(caption("来自 Alex · 3 个文件 · 共 24 MB")),
                    )
                    .child(badge("分享者在线", Tone::Mint)),
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
                            .border_color(border())
                            .child(icon_tile(
                                symbol,
                                if index == 2 {
                                    Tone::Lavender
                                } else {
                                    Tone::Peach
                                },
                                38.,
                            ))
                            .child(label(name, 16.).flex_1())
                            .child(caption(size))
                    },
                )),
            )
            .child(
                row()
                    .gap(px(8.))
                    .child(icon(Icon::Folder, 17.).text_color(muted()))
                    .child(caption("保存到 下载 / 京都的周末")),
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

    fn receiving_files(&self, operation: usize, layout: FlowLayout, cx: &Context<Self>) -> Div {
        let progress_bar = div()
            .h(px(7.))
            .w(relative(0.05))
            .rounded_full()
            .bg(Tone::Lavender.accent());
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
            .child(heading("文件正在向你靠近", 26.))
            .child(label("京都的周末 · 3 个文件", 16.).text_color(muted()))
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
                            .bg(Tone::Lavender.surface())
                            .overflow_hidden()
                            .child(progress_bar),
                    )
                    .child(caption("正在演示接收过程").text_center()),
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

    fn receive_complete(&self, layout: FlowLayout, cx: &Context<Self>) -> Div {
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
                    .child(heading("接收完成", 26.))
                    .child(label("京都的周末 · 3 个文件 · 24 MB", 16.).text_color(muted())),
                "receive-complete",
                self.reduced_motion(),
            ))
            .child(badge("演示完成，未写入实际文件", Tone::Lavender))
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
