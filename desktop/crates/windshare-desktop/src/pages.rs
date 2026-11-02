use crate::localization::{Language, Text, language};
use crate::{
    desktop::Desktop,
    model::{Page, PreviewAction},
};
use gpui::{Div, KeyDownEvent, prelude::*, px};
use windshare_ui::{Button, ButtonStyle, Icon, Theme, Tone, column, row};

impl Desktop {
    pub fn settings_page(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let settings = [
            (
                locale.text(Text::Notifications),
                locale.text(Text::NotificationsDetail),
                Icon::Bell,
            ),
            (
                locale.text(Text::TransferDetails),
                locale.text(Text::TransferDetailsDescription),
                Icon::Activity,
            ),
            (
                locale.text(Text::ReducedMotion),
                locale.text(Text::ReducedMotionDetail),
                Icon::Sun,
            ),
        ];
        column()
            .gap(px(24.))
            .child(Self::language_card(theme, cx))
            .child(
                theme
                    .card()
                    .p(px(28.))
                    .gap(px(10.))
                    .child(theme.heading(locale.text(Text::InterfaceExperience), 22.))
                    .children(settings.into_iter().enumerate().map(
                        |(index, (title, detail, symbol))| {
                            row()
                                .gap(px(18.))
                                .py(px(24.))
                                .when(index > 0, |el| el.border_t_1().border_color(theme.border()))
                                .child(theme.icon_tile(symbol, Tone::Neutral, 44.))
                                .child(
                                    column()
                                        .flex_1()
                                        .gap(px(7.))
                                        .child(theme.heading(title, 17.))
                                        .child(theme.caption(detail)),
                                )
                                .child(
                                    theme
                                        .toggle(("setting", index), self.settings[index])
                                        .aria_label(title)
                                        .on_click(cx.listener(move |this, _, window, cx| {
                                            this.dispatch(
                                                PreviewAction::ToggleSetting(index),
                                                window,
                                                cx,
                                            );
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
                            .child(theme.heading(locale.text(Text::DesignComponents), 18.))
                            .child(theme.caption(locale.text(Text::DesignComponentsDetail))),
                    )
                    .child(Self::button(
                        "view-components",
                        locale.text(Text::ViewComponents),
                        Icon::Grid,
                        ButtonStyle::Secondary,
                        PreviewAction::Navigate(Page::Components),
                        cx,
                    )),
            )
    }

    pub fn components_page(theme: Theme, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        column()
            .gap(px(24.))
            .child(color_palette(theme, locale))
            .child(Self::component_buttons(theme, cx))
            .child(
                theme
                    .card()
                    .p(px(28.))
                    .gap(px(20.))
                    .child(theme.heading(locale.text(Text::TypographyTitle), 28.))
                    .child(
                        theme
                            .label(locale.text(Text::TypographyDetail), 16.)
                            .text_color(theme.muted_text()),
                    )
                    .child(theme.separator())
                    .child(theme.heading(locale.text(Text::StatusSamples), 20.))
                    .child(
                        row()
                            .gap(px(12.))
                            .child(theme.badge(locale.text(Text::Connected), Tone::Mint))
                            .child(theme.badge(locale.text(Text::Waiting), Tone::Peach))
                            .child(theme.badge(locale.text(Text::Paused), Tone::Neutral))
                            .child(theme.badge(locale.text(Text::RetryNeeded), Tone::Rose)),
                    )
                    .child(theme.progress(0.62, Tone::Lavender)),
            )
            .child(
                theme
                    .card()
                    .p(px(28.))
                    .gap(px(18.))
                    .child(
                        theme.section_header(locale.text(Text::Icons), locale.text(Text::IconGrid)),
                    )
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
                            .map(|symbol| theme.icon_tile(symbol, Tone::Neutral, 46.)),
                        ),
                    )
                    .child(theme.caption(locale.text(Text::ComponentsFootnote))),
            )
    }
    fn component_buttons(theme: Theme, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        theme
            .card()
            .p(px(28.))
            .gap(px(22.))
            .child(
                theme.section_header(locale.text(Text::Buttons), locale.text(Text::ButtonStates)),
            )
            .child(
                row()
                    .flex_wrap()
                    .gap(px(12.))
                    .child(Self::button(
                        "sample-primary",
                        locale.text(Text::CopyLink),
                        Icon::Copy,
                        ButtonStyle::Primary,
                        PreviewAction::Toast(Text::LinkCopied),
                        cx,
                    ))
                    .child(Self::button(
                        "sample-secondary",
                        locale.text(Text::ReceiveTitle),
                        Icon::Download,
                        ButtonStyle::Secondary,
                        PreviewAction::Receive,
                        cx,
                    ))
                    .child(Self::button(
                        "sample-tinted",
                        locale.text(Text::NotificationSample),
                        Icon::Bell,
                        ButtonStyle::Tinted(Tone::Lavender),
                        PreviewAction::Toast(Text::NotificationSampleMessage),
                        cx,
                    ))
                    .child(
                        Button::new("sample-disabled", locale.text(Text::Unavailable))
                            .icon(Icon::Lock)
                            .disabled(true),
                    ),
            )
    }
}

fn color_palette(theme: Theme, locale: Language) -> Div {
    row().gap(px(12.)).children(
        [
            (Tone::Mint, locale.text(Text::Mint)),
            (Tone::Lavender, locale.text(Text::Lavender)),
            (Tone::Peach, locale.text(Text::Peach)),
            (Tone::Blue, locale.text(Text::Sky)),
            (Tone::Rose, locale.text(Text::Rose)),
        ]
        .into_iter()
        .map(|(tone, name)| {
            column()
                .flex_1()
                .h(px(110.))
                .p(px(20.))
                .rounded(px(16.))
                .justify_end()
                .bg(tone.surface(theme))
                .child(theme.heading(name, 16.).text_color(tone.text(theme)))
        }),
    )
}
