use crate::{desktop::Desktop, flow_state::*, layout::FlowLayout, model::*, motion::*};
use gpui::{prelude::*, *};
use windshare_ui::*;

impl Desktop {
    pub fn sharing_page(&self, layout: FlowLayout, cx: &Context<Self>) -> Div {
        let content = match self.share_flow {
            ShareFlow::Choose => self.choose_share(layout, cx),
            ShareFlow::Preparing { bundle, .. } => self.preparing_share(bundle, layout, cx),
            ShareFlow::Ready { share_id } => self.ready_share(share_id, cx),
        };
        column()
            .gap(px(22.))
            .child(
                card().rounded(px(24.)).p(px(layout.panel_padding())).child(
                    column()
                        .min_h(px(layout.stage_height()))
                        .justify_center()
                        .child(content),
                ),
            )
            .child(
                row()
                    .justify_center()
                    .flex_wrap()
                    .gap(px(22.))
                    .child(reassurance(Icon::Shield, "端到端加密"))
                    .child(reassurance(Icon::Globe, "对方无需安装"))
                    .child(reassurance(Icon::Monitor, "文件留在你的设备")),
            )
    }

    fn choose_share(&self, layout: FlowLayout, cx: &Context<Self>) -> Div {
        column()
            .items_center()
            .gap(px(16.))
            .child(scaled_spirit(
                SpiritMood::Idle,
                Tone::Mint,
                layout.scene_scale(),
                self.reduced_motion(),
            ))
            .child(
                column()
                    .items_center()
                    .gap(px(8.))
                    .child(heading("想分享什么？", 26.))
                    .child(label("选一份文件，或一整个文件夹。", 16.).text_color(muted())),
            )
            .child(
                row()
                    .gap(px(12.))
                    .child(
                        self.button(
                            "choose-files",
                            "选择文件",
                            Icon::Plus,
                            ButtonStyle::Primary,
                            PreviewAction::SelectFiles,
                            cx,
                        )
                        .large()
                        .focus_handle(&self.primary_focus),
                    )
                    .child(
                        self.button(
                            "choose-folder",
                            "选择文件夹",
                            Icon::Folder,
                            ButtonStyle::Secondary,
                            PreviewAction::SelectFolder,
                            cx,
                        )
                        .large(),
                    ),
            )
            .child(caption("此预览使用示例文件"))
    }

    fn preparing_share(&self, bundle: SampleBundle, layout: FlowLayout, cx: &Context<Self>) -> Div {
        column()
            .items_center()
            .gap(px(16.))
            .child(scaled_spirit(
                SpiritMood::Sending,
                Tone::Mint,
                layout.scene_scale(),
                self.reduced_motion(),
            ))
            .child(
                column()
                    .items_center()
                    .gap(px(8.))
                    .child(heading("正在准备分享", 26.))
                    .child(label("正在建立安全连接，马上就好。", 16.).text_color(muted())),
            )
            .child(
                row()
                    .gap(px(12.))
                    .px(px(20.))
                    .py(px(12.))
                    .rounded(px(12.))
                    .bg(canvas_color())
                    .child(icon(bundle.icon(), 20.).text_color(bundle.tone().text()))
                    .child(label(bundle.name(), 16.))
                    .child(caption(bundle.size())),
            )
            .child(
                self.button(
                    "cancel-share",
                    "取消",
                    Icon::Close,
                    ButtonStyle::Ghost,
                    PreviewAction::ResetShare,
                    cx,
                )
                .compact()
                .focus_handle(&self.primary_focus),
            )
    }

    fn ready_share(&self, id: usize, cx: &Context<Self>) -> Div {
        let Some(share) = self.shares.iter().find(|share| share.id == id) else {
            return column();
        };
        let copied =
            matches!(self.copy_feedback, CopyFeedback::Copied { share_id, .. } if share_id == id);
        column()
            .items_center()
            .gap(px(16.))
            .child(reveal(
                row()
                    .w_full()
                    .max_w(px(620.))
                    .gap(px(22.))
                    .child(scaled_spirit(
                        SpiritMood::Happy,
                        Tone::Mint,
                        0.55,
                        self.reduced_motion(),
                    ))
                    .child(
                        column()
                            .gap(px(8.))
                            .child(heading("链接准备好了", 26.))
                            .child(label("复制链接，发给想分享的人。", 16.).text_color(muted())),
                    ),
                ("share-ready", id),
                self.reduced_motion(),
            ))
            .child(
                column()
                    .w_full()
                    .max_w(px(620.))
                    .gap(px(16.))
                    .child(
                        row()
                            .gap(px(12.))
                            .p(px(12.))
                            .pl(px(18.))
                            .rounded(px(15.))
                            .bg(Tone::Mint.surface())
                            .child(icon(Icon::Link, 21.).text_color(primary()))
                            .child(
                                label(share.link(), 16.)
                                    .font_family(FONT)
                                    .flex_1()
                                    .min_w_0()
                                    .truncate(),
                            )
                            .child(
                                self.button(
                                    "ready-copy",
                                    if copied { "已复制" } else { "复制链接" },
                                    if copied { Icon::Check } else { Icon::Copy },
                                    ButtonStyle::Primary,
                                    PreviewAction::CopyLink(id),
                                    cx,
                                )
                                .large()
                                .min_width(px(128.))
                                .focus_handle(&self.primary_focus),
                            ),
                    )
                    .child(
                        row()
                            .gap(px(12.))
                            .p(px(12.))
                            .rounded(px(12.))
                            .border_1()
                            .border_color(border())
                            .child(icon_tile(share.symbol, share.tone, 38.))
                            .child(label(share.name.clone(), 16.).flex_1())
                            .child(caption(format!("{} · {}", share.detail, share.size))),
                    ),
            )
            .child(reassurance(
                Icon::Monitor,
                "保持 WindShare 开启，对方就能接收。",
            ))
            .child(
                self.button(
                    "share-again",
                    "再分享一组",
                    Icon::Plus,
                    ButtonStyle::Ghost,
                    PreviewAction::ResetShare,
                    cx,
                )
                .compact(),
            )
    }
}
