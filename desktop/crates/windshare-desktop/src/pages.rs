use crate::localization::{Language, Text, language};
use crate::{
    desktop::Desktop,
    model::{Action, Page, SettingsSection},
    preferences::{NotificationsPreference, NotificationsSettings, Persistence},
};
use gpui::{Animation, AnimationExt, Div, ease_out_quint, prelude::*, px};
use std::time::Duration;
use windshare_ui::{Button, ButtonStyle, Icon, Theme, Tone, column, icon, row};

const TAB_TRANSITION_MS: u64 = 180;

impl Desktop {
    pub fn active_settings_section(&self, cx: &gpui::App) -> SettingsSection {
        if self.settings_section_available(self.settings_section, cx) {
            self.settings_section
        } else {
            SettingsSection::General
        }
    }

    pub fn settings_section_body(
        &self,
        theme: Theme,
        active: SettingsSection,
        compact: bool,
        cx: &Context<Self>,
    ) -> impl IntoElement {
        let body = match active {
            SettingsSection::General => self.general_settings(theme, cx),
            SettingsSection::Network => self.network_settings_page(theme, compact, cx),
            SettingsSection::Account => self.account_settings_page(theme, cx),
            SettingsSection::Diagnostics => self.diagnostics_card(theme, cx),
        };
        body.with_animation(
            ("settings-section", active as usize),
            Animation::new(Duration::from_millis(if cx.reduce_motion() {
                1
            } else {
                TAB_TRANSITION_MS
            }))
            .with_easing(ease_out_quint()),
            gpui::Styled::opacity,
        )
    }

    pub fn settings_tabs(&self, theme: Theme, active: SettingsSection, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let sections = [
            SettingsSection::General,
            SettingsSection::Network,
            SettingsSection::Account,
            SettingsSection::Diagnostics,
        ];
        row().child(
            row()
                .flex_wrap()
                .gap(px(4.))
                .p(px(4.))
                .rounded(px(12.))
                .bg(theme.inset())
                .children(
                    sections
                        .into_iter()
                        .filter(|section| self.settings_section_available(*section, cx))
                        .map(|section| {
                            self.settings_tab(theme, section, active == section, locale, cx)
                        }),
                ),
        )
    }

    fn settings_tab(
        &self,
        theme: Theme,
        section: SettingsSection,
        selected: bool,
        locale: Language,
        cx: &Context<Self>,
    ) -> Button {
        let attention = match section {
            SettingsSection::Network => self.network_attention_tone(cx),
            SettingsSection::Account => self.account_attention_tone(cx),
            SettingsSection::Diagnostics => self.diagnostics_attention_tone(cx),
            SettingsSection::General => None,
        };
        let icon_color = if selected {
            theme.text()
        } else {
            theme.muted_text()
        };
        let button = Button::new(("settings-tab", section as usize), section.title(locale))
            .style(if selected {
                ButtonStyle::Secondary
            } else {
                ButtonStyle::Ghost
            })
            .compact()
            .on_press(cx.listener(move |this, (), window, cx| {
                this.dispatch(Action::SettingsSection(section), window, cx);
            }));
        if let Some(tone) = attention {
            button.leading(
                row()
                    .gap(px(6.))
                    .child(icon(section.icon(), 16.).text_color(icon_color))
                    .child(theme.dot(tone)),
            )
        } else {
            button.icon(section.icon())
        }
    }

    fn general_settings(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        column()
            .gap(px(20.))
            .child(Self::appearance_card(theme, cx))
            .child(Self::language_card(theme, cx))
            .child(
                theme
                    .card()
                    .p(px(28.))
                    .gap(px(10.))
                    .child(theme.heading(locale.text(Text::InterfaceExperience), 22.))
                    .child(Self::share_animation_setting(theme, cx))
                    .child(Self::notifications_setting(theme, cx))
                    .when(self.runtime.is_preview(), |el| {
                        el.child(self.transfer_details_setting(theme, cx))
                    }),
            )
            .when(self.runtime.is_preview(), |el| {
                el.child(Self::component_preview(theme, cx))
            })
    }

    fn notifications_setting(theme: Theme, cx: &Context<Self>) -> Div {
        let settings = cx.global::<NotificationsSettings>();
        let locale = language(cx);
        let enabled = settings.choice.enabled();
        let title = locale.text(Text::Notifications);
        column()
            .py(px(24.))
            .gap(px(12.))
            .border_t_1()
            .border_color(theme.border())
            .child(
                row()
                    .gap(px(18.))
                    .child(theme.icon_tile(Icon::Bell, Tone::Neutral, 44.))
                    .child(
                        column()
                            .flex_1()
                            .gap(px(7.))
                            .child(theme.heading(title, 17.))
                            .child(theme.caption(locale.text(Text::NotificationsDetail))),
                    )
                    .child(Self::setting_switch(
                        theme,
                        "notifications-setting",
                        title,
                        enabled,
                        Some(Action::SetNotifications(
                            NotificationsPreference::from_enabled(!enabled),
                        )),
                        cx,
                    )),
            )
            .when(settings.persistence != Persistence::Saved, |el| {
                let message = match settings.persistence {
                    Persistence::LoadFailed => Text::NotificationsLoadFailed,
                    _ => Text::NotificationsSaveFailed,
                };
                el.child(
                    row()
                        .flex_wrap()
                        .gap(px(12.))
                        .child(
                            theme
                                .caption(locale.text(message))
                                .text_color(Tone::Rose.text(theme)),
                        )
                        .child(
                            Self::button(
                                "retry-notifications",
                                locale.text(Text::RetrySave),
                                Icon::Settings,
                                ButtonStyle::Secondary,
                                Action::SetNotifications(settings.choice),
                                cx,
                            )
                            .compact(),
                        ),
                )
            })
    }

    fn transfer_details_setting(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let title = locale.text(Text::TransferDetails);
        row()
            .gap(px(18.))
            .py(px(24.))
            .border_t_1()
            .border_color(theme.border())
            .child(theme.icon_tile(Icon::Activity, Tone::Neutral, 44.))
            .child(
                column()
                    .flex_1()
                    .gap(px(7.))
                    .child(theme.heading(title, 17.))
                    .child(theme.caption(locale.text(Text::TransferDetailsDescription))),
            )
            .child(Self::setting_switch(
                theme,
                "transfer-details-setting",
                title,
                self.preview_transfer_details,
                Some(Action::TogglePreviewTransferDetails),
                cx,
            ))
    }

    fn component_preview(theme: Theme, cx: &Context<Self>) -> Div {
        let locale = language(cx);
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
                Action::Navigate(Page::Components),
                cx,
            ))
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
                        Action::Toast(Text::LinkCopied),
                        cx,
                    ))
                    .child(Self::button(
                        "sample-secondary",
                        locale.text(Text::ReceiveTitle),
                        Icon::Download,
                        ButtonStyle::Secondary,
                        Action::Receive,
                        cx,
                    ))
                    .child(Self::button(
                        "sample-tinted",
                        locale.text(Text::NotificationSample),
                        Icon::Bell,
                        ButtonStyle::Tinted(Tone::Lavender),
                        Action::Toast(Text::NotificationSampleMessage),
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
