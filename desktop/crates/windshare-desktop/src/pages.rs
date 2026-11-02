use crate::{desktop::Desktop, model::*};
use gpui::{prelude::*, *};
use windshare_ui::*;

impl Desktop {
    pub fn settings_page(&self, cx: &Context<Self>) -> Div {
        let settings = [
            ("轻提示", "操作完成后显示轻量通知。", Icon::Bell),
            ("传输详情", "显示速度与预计剩余时间。", Icon::Activity),
            ("减少动态效果", "关闭装饰动画，使用即时过渡。", Icon::Sun),
        ];
        column()
            .gap(px(24.))
            .child(
                card()
                    .p(px(28.))
                    .gap(px(10.))
                    .child(heading("界面与体验", 22.))
                    .children(settings.into_iter().enumerate().map(
                        |(index, (title, detail, symbol))| {
                            row()
                                .gap(px(18.))
                                .py(px(24.))
                                .when(index > 0, |el| el.border_t_1().border_color(border()))
                                .child(icon_tile(symbol, Tone::Neutral, 44.))
                                .child(
                                    column()
                                        .flex_1()
                                        .gap(px(7.))
                                        .child(heading(title, 17.))
                                        .child(caption(detail)),
                                )
                                .child(
                                    toggle(("setting", index), self.settings[index])
                                        .aria_label(title)
                                        .on_click(cx.listener(move |this, _, window, cx| {
                                            this.dispatch(
                                                PreviewAction::ToggleSetting(index),
                                                window,
                                                cx,
                                            )
                                        }))
                                        .on_key_down(cx.listener(
                                            move |this, event: &KeyDownEvent, window, cx| {
                                                if matches!(
                                                    event.keystroke.key.as_str(),
                                                    "enter" | "space"
                                                ) {
                                                    cx.stop_propagation();
                                                    this.dispatch(
                                                        PreviewAction::ToggleSetting(index),
                                                        window,
                                                        cx,
                                                    );
                                                }
                                            },
                                        )),
                                )
                        },
                    )),
            )
            .child(
                row()
                    .justify_between()
                    .gap(px(16.))
                    .p(px(24.))
                    .child(
                        column()
                            .gap(px(7.))
                            .child(heading("WindShare 设计组件", 18.))
                            .child(caption("柔和配色、清晰字号，以及原生的细节。")),
                    )
                    .child(self.button(
                        "view-components",
                        "查看组件",
                        Icon::Grid,
                        ButtonStyle::Secondary,
                        PreviewAction::Navigate(Page::Components),
                        cx,
                    )),
            )
    }

    pub fn components_page(&self, cx: &Context<Self>) -> Div {
        column()
            .gap(px(24.))
            .child(
                row().gap(px(12.)).children(
                    [
                        (Tone::Mint, "薄荷"),
                        (Tone::Lavender, "薰衣草"),
                        (Tone::Peach, "杏桃"),
                        (Tone::Blue, "晴空"),
                        (Tone::Rose, "玫瑰"),
                    ]
                    .into_iter()
                    .map(|(tone, name)| {
                        column()
                            .flex_1()
                            .h(px(110.))
                            .p(px(20.))
                            .rounded(px(16.))
                            .justify_end()
                            .bg(tone.surface())
                            .child(heading(name, 16.).text_color(tone.text()))
                    }),
                ),
            )
            .child(
                card()
                    .p(px(28.))
                    .gap(px(22.))
                    .child(section_header("按钮", "悬停 · 聚焦 · 按下"))
                    .child(
                        row()
                            .flex_wrap()
                            .gap(px(12.))
                            .child(self.button(
                                "sample-primary",
                                "复制链接",
                                Icon::Copy,
                                ButtonStyle::Primary,
                                PreviewAction::Toast("分享链接已复制"),
                                cx,
                            ))
                            .child(self.button(
                                "sample-secondary",
                                "接收文件",
                                Icon::Download,
                                ButtonStyle::Secondary,
                                PreviewAction::Receive,
                                cx,
                            ))
                            .child(self.button(
                                "sample-tinted",
                                "轻量通知",
                                Icon::Bell,
                                ButtonStyle::Tinted(Tone::Lavender),
                                PreviewAction::Toast("这是一条薄荷绿轻提示"),
                                cx,
                            ))
                            .child(
                                Button::new("sample-disabled", "暂不可用")
                                    .icon(Icon::Lock)
                                    .disabled(true),
                            ),
                    ),
            )
            .child(
                card()
                    .p(px(28.))
                    .gap(px(20.))
                    .child(heading("让信息清晰，让阅读轻松", 28.))
                    .child(
                        label("正文 16 px · 辅助文字 14 px · 页面标题 34 px", 16.)
                            .text_color(muted()),
                    )
                    .child(separator())
                    .child(heading("状态也有自己的秩序", 20.))
                    .child(
                        row()
                            .gap(px(12.))
                            .child(badge("已连接", Tone::Mint))
                            .child(badge("等待中", Tone::Peach))
                            .child(badge("已暂停", Tone::Neutral))
                            .child(badge("需要重试", Tone::Rose)),
                    )
                    .child(progress(0.62, Tone::Lavender)),
            )
            .child(
                card()
                    .p(px(28.))
                    .gap(px(18.))
                    .child(section_header("自有图标", "24 px 网格"))
                    .child(
                        row().flex_wrap().gap(px(16.)).children(
                            [
                                Icon::Wind,
                                Icon::Folder,
                                Icon::Link,
                                Icon::Shield,
                                Icon::Globe,
                                Icon::Download,
                                Icon::Image,
                                Icon::Music,
                                Icon::Heart,
                                Icon::Spark,
                                Icon::Monitor,
                                Icon::Sun,
                            ]
                            .into_iter()
                            .map(|symbol| icon_tile(symbol, Tone::Neutral, 46.)),
                        ),
                    )
                    .child(caption("所有控件基于 GPUI 基础元素实现。")),
            )
    }
}
