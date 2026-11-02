use crate::{
    desktop::Desktop,
    model::{Page, PreviewAction},
};
use gpui::{Div, MouseButton, WindowControlArea, div, img, prelude::*, px};
use windshare_ui::{
    Button, ButtonStyle, FONT, Icon, SIDEBAR_COMPACT_WIDTH, SIDEBAR_WIDTH, Theme, Tone, column, row,
};

const TITLEBAR_HEIGHT: f32 = 40.;

impl Desktop {
    fn titlebar_drag_area() -> Div {
        div()
            .h(px(TITLEBAR_HEIGHT))
            .flex_shrink_0()
            .window_control_area(WindowControlArea::Drag)
            .on_mouse_down(MouseButton::Left, |event, window, _| {
                if event.click_count == 2 {
                    window.zoom_window();
                } else {
                    window.start_window_move();
                }
            })
    }

    pub fn titlebar(theme: Theme) -> Div {
        row()
            .h(px(TITLEBAR_HEIGHT))
            .flex_shrink_0()
            .pl(px(16.))
            .pr(px(5.))
            .bg(theme.canvas())
            .child(Self::titlebar_drag_area().flex_1())
            .when(cfg!(not(target_os = "macos")), |el| {
                el.child(
                    row()
                        .gap_1()
                        .child(
                            Button::new("minimize", "")
                                .icon(Icon::Minimize)
                                .compact()
                                .style(ButtonStyle::Ghost)
                                .tooltip("最小化")
                                .on_press(|(), window, _| window.minimize_window()),
                        )
                        .child(
                            Button::new("maximize", "")
                                .icon(Icon::Maximize)
                                .compact()
                                .style(ButtonStyle::Ghost)
                                .tooltip("最大化或还原")
                                .on_press(|(), window, _| window.zoom_window()),
                        )
                        .child(
                            Button::new("close-window", "")
                                .icon(Icon::Close)
                                .compact()
                                .style(ButtonStyle::Ghost)
                                .tooltip("关闭窗口")
                                .on_press(|(), window, _| window.remove_window()),
                        ),
                )
            })
    }

    pub fn sidebar(&self, theme: Theme, compact: bool, cx: &Context<Self>) -> Div {
        column()
            .w(px(if compact {
                SIDEBAR_COMPACT_WIDTH
            } else {
                SIDEBAR_WIDTH
            }))
            .h_full()
            .flex_shrink_0()
            .bg(theme.sidebar())
            .border_r_1()
            .border_color(theme.border())
            .child(Self::titlebar_drag_area().w_full())
            .child(self.sidebar_content(theme, compact, cx))
    }

    fn sidebar_content(&self, theme: Theme, compact: bool, cx: &Context<Self>) -> Div {
        column()
            .flex_1()
            .min_h_0()
            .px(px(if compact { 13. } else { 18. }))
            .pt(px(22.))
            .pb(px(22.))
            .child(
                row()
                    .gap(px(8.))
                    .mb(px(42.))
                    .child(
                        img("brand/spirit.svg")
                            .size(px(if compact { 34. } else { 42. }))
                            .flex_shrink_0(),
                    )
                    .child(
                        theme
                            .heading("WindShare", if compact { 18. } else { 22. })
                            .font_family(FONT)
                            .text_color(theme.accent_text()),
                    ),
            )
            .child(
                column()
                    .gap(px(10.))
                    .child(self.navigation(Page::Share, Icon::Upload, cx))
                    .child(self.navigation(Page::Receive, Icon::Download, cx)),
            )
            .child(div().my(px(25.)).child(theme.separator()))
            .child(
                column()
                    .gap(px(8.))
                    .child(self.navigation(Page::Shares, Icon::Folder, cx))
                    .child(self.navigation(Page::Transfers, Icon::Activity, cx)),
            )
            .child(div().flex_1().min_h(px(40.)))
            .child(self.navigation(Page::Settings, Icon::Settings, cx))
            .child(
                Self::button(
                    "about-nav",
                    if compact {
                        "关于"
                    } else {
                        "关于 WindShare"
                    },
                    Icon::Help,
                    ButtonStyle::Navigation,
                    PreviewAction::Help,
                    cx,
                )
                .full_width(),
            )
    }

    fn navigation(&self, page: Page, symbol: Icon, cx: &Context<Self>) -> Button {
        Self::button(
            ("nav", page as usize),
            page.title(),
            symbol,
            if self.page == page {
                ButtonStyle::ActiveNavigation(if page == Page::Receive {
                    Tone::Lavender
                } else {
                    Tone::Mint
                })
            } else {
                ButtonStyle::Navigation
            },
            PreviewAction::Navigate(page),
            cx,
        )
        .full_width()
        .large()
    }

    pub fn page_heading(&self, theme: Theme, compact: bool, cx: &Context<Self>) -> Div {
        row()
            .justify_between()
            .gap(px(20.))
            .child(
                column()
                    .gap(px(10.))
                    .child(theme.heading(self.page.title(), if compact { 30. } else { 34. }))
                    .child(
                        theme
                            .label(self.page.subtitle(), 16.)
                            .text_color(theme.muted_text())
                            .line_height(px(26.)),
                    ),
            )
            .when(self.page == Page::Shares, |el| {
                el.child(
                    Self::button(
                        "new-share-top",
                        "新建分享",
                        Icon::Plus,
                        ButtonStyle::Primary,
                        PreviewAction::NewShare,
                        cx,
                    )
                    .tooltip("新建分享 · Ctrl+N"),
                )
            })
    }

    pub fn statusbar(theme: Theme) -> Div {
        row()
            .h(px(40.))
            .flex_shrink_0()
            .px(px(28.))
            .gap(px(8.))
            .child(theme.dot(Tone::Peach))
            .child(
                theme
                    .label("UI 预览 · 示例文件", 12.)
                    .text_color(theme.muted_text()),
            )
    }
}
