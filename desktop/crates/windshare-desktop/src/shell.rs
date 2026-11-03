use crate::localization::{Text, language};
use crate::{
    artwork::Artwork,
    desktop::Desktop,
    model::{Page, PreviewAction},
};
use gpui::{Div, Window, div, point, prelude::*, px};
use windshare_ui::{
    Button, ButtonStyle, FONT, Icon, SIDEBAR_COMPACT_WIDTH, SIDEBAR_WIDTH, Theme, Tone, column,
    row, toggle_window_maximize, window_drag_area,
};

const TITLEBAR_HEIGHT: f32 = 40.;
const BRAND_MARK_SIZE: f32 = 42.;
const COMPACT_BRAND_MARK_SIZE: f32 = 36.;

impl Desktop {
    fn titlebar_drag_area() -> Div {
        window_drag_area().h(px(TITLEBAR_HEIGHT)).flex_shrink_0()
    }

    pub fn titlebar(theme: Theme, window: &Window, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let (maximize_icon, maximize_label) = if window.is_maximized() {
            (Icon::Restore, Text::RestoreWindow)
        } else {
            (Icon::Maximize, Text::Maximize)
        };
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
                                .tooltip(locale.text(Text::Minimize))
                                .on_press(|(), window, _| window.minimize_window()),
                        )
                        .child(
                            Button::new("maximize", "")
                                .icon(maximize_icon)
                                .compact()
                                .style(ButtonStyle::Ghost)
                                .tooltip(locale.text(maximize_label))
                                .on_press(|(), window, _| toggle_window_maximize(window)),
                        )
                        .child(
                            Button::new("close-window", "")
                                .icon(Icon::Close)
                                .compact()
                                .style(ButtonStyle::Ghost)
                                .tooltip(locale.text(Text::CloseWindow))
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
        let locale = language(cx);
        let mark_size = if compact {
            COMPACT_BRAND_MARK_SIZE
        } else {
            BRAND_MARK_SIZE
        };
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
                    .child(div().relative().size(px(mark_size)).flex_shrink_0().child(
                        Artwork::BrandMark.sprite(point(mark_size / 2., mark_size / 2.), mark_size),
                    ))
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
                        locale.text(Text::AboutShort)
                    } else {
                        locale.text(Text::About)
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
        let locale = language(cx);
        Self::button(
            ("nav", page as usize),
            page.title(locale),
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
        let locale = language(cx);
        row()
            .justify_between()
            .gap(px(20.))
            .child(
                column()
                    .flex_1()
                    .min_w_0()
                    .gap(px(10.))
                    .child(theme.heading(self.page.title(locale), if compact { 30. } else { 34. }))
                    .child(
                        theme
                            .label(self.page.subtitle(locale), 16.)
                            .text_color(theme.muted_text())
                            .line_height(px(26.)),
                    ),
            )
            .when(self.page == Page::Shares, |el| {
                el.child(
                    Self::button(
                        "new-share-top",
                        locale.text(Text::NewShare),
                        Icon::Plus,
                        ButtonStyle::Primary,
                        PreviewAction::NewShare,
                        cx,
                    )
                    .tooltip(locale.text(Text::NewShareShortcut)),
                )
            })
    }

    pub fn statusbar(theme: Theme, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        row()
            .h(px(40.))
            .flex_shrink_0()
            .px(px(28.))
            .gap(px(8.))
            .child(theme.dot(Tone::Peach))
            .child(
                theme
                    .label(locale.text(Text::PreviewStatus), 12.)
                    .text_color(theme.muted_text()),
            )
    }
}
