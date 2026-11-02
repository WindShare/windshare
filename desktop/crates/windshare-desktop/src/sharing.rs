use crate::{
    desktop::Desktop,
    flow_state::{CopyFeedback, SampleBundle, ShareFlow, ShareRun},
    layout::FlowLayout,
    model::PreviewAction,
    motion::{SpiritMood, reassurance, scaled_spirit},
    share_animation::{SHARE_LINK_HEIGHT, ShareGeometry, ShareScene},
    share_timing::lerp,
};
use gpui::{Div, Window, div, prelude::*, px};
use windshare_ui::{
    Button, ButtonStyle, FONT, Icon, Tone, border, caption, card, column, heading, icon, icon_tile,
    label, muted, paper, primary, row,
};

impl Desktop {
    pub fn sharing_page(&mut self, layout: FlowLayout, window: &Window, cx: &Context<Self>) -> Div {
        let geometry = ShareGeometry::new(window, layout);
        let still = self.reduced_motion() || cx.reduce_motion();
        let active = window.is_window_active();
        let content = match self.share_flow {
            ShareFlow::Choose => column()
                .h(px(geometry.height))
                .justify_center()
                .child(self.choose_share(layout, still || !active, cx)),
            ShareFlow::Running(run) => {
                let now = std::time::Instant::now();
                let frame = run.sample(now, still);
                self.motion_probe.observe(run, frame, now, active && !still);
                if active && !still {
                    window.request_animation_frame();
                }
                let scene = ShareScene::new(geometry, frame);
                if still && run.share_id().is_none() {
                    self.quiet_preparation(run.bundle, layout, cx)
                } else if scene.landing() {
                    self.result_share(run, scene, cx)
                } else {
                    self.preparing_scene(run.bundle, scene, cx)
                }
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
    fn choose_share(&self, layout: FlowLayout, still: bool, cx: &Context<Self>) -> Div {
        column()
            .items_center()
            .gap(px(16.))
            .child(scaled_spirit(
                SpiritMood::Idle,
                Tone::Mint,
                layout.scene_scale(),
                still,
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
                        Self::button(
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
                        Self::button(
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
            .child(scene.scenery())
            .child(preparation_message(bundle.name(), visibility))
            .child(
                row()
                    .absolute()
                    .bottom_0()
                    .w_full()
                    .justify_center()
                    .opacity(visibility)
                    .child(
                        Self::button(
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
                Self::button(
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

    fn result_share(&self, run: ShareRun, scene: ShareScene, cx: &Context<Self>) -> Div {
        let g = scene.geometry;
        let title = scene.title_visibility();
        let summary = scene.summary_visibility();
        let footer = scene.footer_visibility();
        let result = scene.frame.result_visibility;
        let pending = run.share_id().is_none();
        let bundle = run.bundle;
        div()
            .relative()
            .w_full()
            .h(px(g.height))
            .child(scene.scenery())
            .child(
                column()
                    .absolute()
                    .left(px(g.content_left))
                    .top(px(g.content_top + lerp(36., 18., title)))
                    .w(px(g.content_width - 196.))
                    .h(px(72.))
                    .opacity(title)
                    .when(result < 1., |el| {
                        el.child(result_title(
                            "正在准备链接",
                            "文件已就位，再等一小会儿。",
                            1. - result,
                        ))
                    })
                    .when(result > 0., |el| {
                        el.child(result_title(
                            "链接准备好了",
                            "复制链接，发给想分享的人。",
                            result,
                        ))
                    }),
            )
            .child(self.link_reveal(run, scene, cx))
            .child(
                row()
                    .absolute()
                    .left(px(g.content_left))
                    .top(px(g.content_top + lerp(236., 212., summary)))
                    .w(px(g.content_width))
                    .h(px(62.))
                    .gap(px(12.))
                    .p(px(12.))
                    .rounded(px(12.))
                    .border_1()
                    .border_color(border())
                    .bg(paper())
                    .opacity(summary)
                    .child(icon_tile(bundle.icon(), bundle.tone(), 38.))
                    .child(label(bundle.name(), 16.).flex_1())
                    .child(caption(format!("{} · {}", bundle.detail(), bundle.size()))),
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
                        Self::button(
                            "share-again",
                            if pending { "取消" } else { "再分享一组" },
                            if pending { Icon::Close } else { Icon::Plus },
                            ButtonStyle::Ghost,
                            PreviewAction::ResetShare,
                            cx,
                        )
                        .compact()
                        .when(pending, |button| button.focus_handle(&self.primary_focus)),
                    ),
            )
    }

    fn link_reveal(&self, run: ShareRun, scene: ShareScene, cx: &Context<Self>) -> Div {
        let g = scene.geometry;
        let expansion = scene.link_expansion();
        let result = scene.frame.result_visibility;
        let share = run
            .share_id()
            .and_then(|id| self.shares.iter().find(|share| share.id == id));
        let copied = matches!(self.copy_feedback, CopyFeedback::Copied { share_id, .. }
            if Some(share_id) == run.share_id());
        let enabled = share.is_some() && scene.control_visibility() > 0.95;
        let control = Button::new(
            "ready-copy",
            if share.is_none() {
                "等待链接"
            } else if copied {
                "已复制"
            } else {
                "复制链接"
            },
        )
        .icon(if copied { Icon::Check } else { Icon::Copy })
        .style(ButtonStyle::Primary)
        .large()
        .min_width(px(128.))
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
        div()
            .absolute()
            .left(px(g.content_left))
            .top(px(g.link_top() + lerp(18., 0., expansion)))
            .w(px(g.content_width))
            .h(px(SHARE_LINK_HEIGHT))
            .child(
                div()
                    .absolute()
                    .left_0()
                    .top_0()
                    .h_full()
                    .w(px(lerp(60., g.content_width, expansion)))
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
                        div()
                            .relative()
                            .h(px(24.))
                            .flex_1()
                            .min_w_0()
                            .opacity(scene.link_visibility())
                            .when(result < 1., |el| el.child(link_placeholder(scene)))
                            .when_some(share, |el, share| {
                                el.child(
                                    label(share.link(), 16.)
                                        .absolute()
                                        .inset_0()
                                        .w_full()
                                        .font_family(FONT)
                                        .truncate()
                                        .opacity(result),
                                )
                            }),
                    )
                    .child(div().opacity(scene.control_visibility()).child(control)),
            )
    }
}

fn result_title(title: &'static str, subtitle: &'static str, visibility: f32) -> Div {
    column()
        .absolute()
        .inset_0()
        .gap(px(8.))
        .opacity(visibility)
        .child(heading(title, 26.))
        .child(label(subtitle, 16.).text_color(muted()))
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

fn link_placeholder(scene: ShareScene) -> Div {
    let result = scene.frame.result_visibility;
    let breath_variation = 0.15 * scene.frame.breath;
    let placeholder_opacity = (1. - result) * (0.65 + breath_variation);
    row()
        .absolute()
        .inset_0()
        .gap(px(8.))
        .opacity(placeholder_opacity)
        .child(
            div()
                .w(px(138.))
                .h(px(8.))
                .rounded_full()
                .bg(Tone::Mint.accent().opacity(0.35)),
        )
        .child(
            div()
                .w(px(44.))
                .h(px(8.))
                .rounded_full()
                .bg(Tone::Mint.accent().opacity(0.20)),
        )
}
