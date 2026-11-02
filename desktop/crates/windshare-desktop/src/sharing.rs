use crate::{
    desktop::Desktop,
    flow_state::*,
    layout::FlowLayout,
    model::*,
    motion::*,
    share_animation::{SHARE_LINK_HEIGHT, ShareGeometry, ShareScene},
};
use gpui::{prelude::*, *};
use windshare_ui::*;

impl Desktop {
    pub fn sharing_page(&self, layout: FlowLayout, window: &mut Window, cx: &Context<Self>) -> Div {
        let geometry = ShareGeometry::new(window, layout);
        let still = self.reduced_motion() || cx.reduce_motion();
        let content = match self.share_flow {
            ShareFlow::Choose => column()
                .h(px(geometry.height))
                .justify_center()
                .child(self.choose_share(layout, cx)),
            ShareFlow::Preparing {
                bundle, started_at, ..
            }
            | ShareFlow::AwaitingReveal {
                bundle, started_at, ..
            } if !still => {
                window.request_animation_frame();
                self.preparing_scene(bundle, ShareScene::flight(geometry, started_at), cx)
            }
            ShareFlow::Preparing { bundle, .. } | ShareFlow::AwaitingReveal { bundle, .. } => {
                self.quiet_preparation(bundle, layout, cx)
            }
            ShareFlow::Revealing {
                share_id, arrival, ..
            } => {
                let scene = if still {
                    ShareScene::settled(geometry)
                } else {
                    window.request_animation_frame();
                    ShareScene::arrival(geometry, arrival)
                };
                self.ready_share(share_id, scene, still, cx)
            }
            ShareFlow::Ready { share_id } => {
                self.ready_share(share_id, ShareScene::settled(geometry), still, cx)
            }
        };
        column()
            .gap(px(22.))
            .child(
                card()
                    .rounded(px(24.))
                    .p(px(layout.panel_padding()))
                    .overflow_hidden()
                    .child(content),
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

    fn preparing_scene(&self, bundle: SampleBundle, scene: ShareScene, cx: &Context<Self>) -> Div {
        let visibility = scene.preparation_visibility();
        div()
            .relative()
            .w_full()
            .h(px(scene.geometry.height))
            .child(scene.scenery(false))
            .child(preparation_message(bundle.name(), visibility))
            .child(
                row()
                    .absolute()
                    .bottom_0()
                    .w_full()
                    .justify_center()
                    .opacity(visibility)
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
                    ),
            )
    }

    fn quiet_preparation(
        &self,
        bundle: SampleBundle,
        layout: FlowLayout,
        cx: &Context<Self>,
    ) -> Div {
        column()
            .h(px(layout.stage_height()))
            .items_center()
            .justify_center()
            .gap(px(16.))
            .child(scaled_spirit(
                SpiritMood::Sending,
                Tone::Mint,
                layout.scene_scale(),
                true,
            ))
            .child(heading("正在准备分享", 26.))
            .child(label(bundle.name(), 16.).text_color(muted()))
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

    fn ready_share(&self, id: usize, scene: ShareScene, still: bool, cx: &Context<Self>) -> Div {
        let Some(share) = self.shares.iter().find(|share| share.id == id) else {
            return column();
        };
        let g = scene.geometry;
        let title = scene.title_visibility();
        let summary = scene.summary_visibility();
        let footer = scene.footer_visibility();
        div()
            .relative()
            .w_full()
            .h(px(g.height))
            .child(scene.scenery(still))
            .when(scene.preparation_visibility() > 0., |el| {
                el.child(preparation_message(
                    &share.name,
                    scene.preparation_visibility(),
                ))
            })
            .child(
                column()
                    .absolute()
                    .left(px(g.content_left))
                    .top(px(g.content_top + 18. + 18. * (1. - title)))
                    .w(px(g.content_width - 196.))
                    .gap(px(8.))
                    .opacity(title)
                    .child(heading("链接准备好了", 26.))
                    .child(label("复制链接，发给想分享的人。", 16.).text_color(muted())),
            )
            .child(self.link_reveal(share, scene, cx))
            .child(
                row()
                    .absolute()
                    .left(px(g.content_left))
                    .top(px(g.content_top + 212. + 24. * (1. - summary)))
                    .w(px(g.content_width))
                    .h(px(62.))
                    .gap(px(12.))
                    .p(px(12.))
                    .rounded(px(12.))
                    .border_1()
                    .border_color(border())
                    .bg(paper())
                    .opacity(summary)
                    .child(icon_tile(share.symbol, share.tone, 38.))
                    .child(label(share.name.clone(), 16.).flex_1())
                    .child(caption(format!("{} · {}", share.detail, share.size))),
            )
            .child(
                row()
                    .absolute()
                    .top(px(g.content_top + 286.))
                    .w_full()
                    .justify_center()
                    .opacity(footer)
                    .child(reassurance(
                        Icon::Monitor,
                        "保持 WindShare 开启，对方就能接收。",
                    )),
            )
            .child(
                row()
                    .absolute()
                    .top(px(g.content_top + 316.))
                    .w_full()
                    .justify_center()
                    .opacity(footer)
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
                    ),
            )
    }

    fn link_reveal(&self, share: &Share, scene: ShareScene, cx: &Context<Self>) -> Div {
        let g = scene.geometry;
        let expansion = scene.link_expansion();
        let copied = matches!(self.copy_feedback, CopyFeedback::Copied { share_id, .. } if share_id == share.id);
        div()
            .absolute()
            .left(px(g.content_left))
            .top(px(g.link_top() + 18. * (1. - expansion)))
            .w(px(g.content_width))
            .h(px(SHARE_LINK_HEIGHT))
            .child(
                div()
                    .absolute()
                    .left_0()
                    .top_0()
                    .h_full()
                    .w(px(60. + (g.content_width - 60.) * expansion))
                    .rounded(px(15.))
                    .bg(Tone::Mint.surface())
                    .opacity(scene.control_visibility()),
            )
            .child(
                row()
                    .size_full()
                    .gap(px(12.))
                    .p(px(12.))
                    .pl(px(18.))
                    .child(
                        icon(Icon::Link, 21.)
                            .text_color(primary())
                            .opacity(scene.link_visibility()),
                    )
                    .child(
                        label(share.link(), 16.)
                            .font_family(FONT)
                            .flex_1()
                            .min_w_0()
                            .truncate()
                            .opacity(scene.link_visibility()),
                    )
                    .child(
                        div().opacity(scene.control_visibility()).child(
                            self.button(
                                "ready-copy",
                                if copied { "已复制" } else { "复制链接" },
                                if copied { Icon::Check } else { Icon::Copy },
                                ButtonStyle::Primary,
                                PreviewAction::CopyLink(share.id),
                                cx,
                            )
                            .large()
                            .min_width(px(128.))
                            .focus_handle(&self.primary_focus),
                        ),
                    ),
            )
    }
}

fn preparation_message(name: &str, visibility: f32) -> Div {
    column()
        .absolute()
        .bottom(px(44.))
        .w_full()
        .items_center()
        .gap(px(8.))
        .opacity(visibility)
        .child(heading("正在准备分享", 26.))
        .child(label(name.to_owned(), 16.).text_color(muted()))
}
