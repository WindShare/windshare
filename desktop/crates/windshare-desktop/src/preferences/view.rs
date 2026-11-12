use super::{AppearanceSettings, Persistence, PreferenceChoice};
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::Action,
};
use gpui::{Div, prelude::*, px};
use windshare_ui::{ButtonStyle, Icon, Theme, ThemePreference, Tone, row};

impl Desktop {
    pub fn appearance_card(theme: Theme, cx: &Context<Self>) -> Div {
        let settings = cx.global::<AppearanceSettings>();
        let locale = language(cx);
        theme
            .card()
            .p(px(28.))
            .gap(px(18.))
            .child(
                row()
                    .gap(px(18.))
                    .child(theme.icon_tile(Icon::Sun, Tone::Mint, 44.))
                    .child(theme.heading(locale.text(Text::Appearance), 22.)),
            )
            .child(
                row()
                    .flex_wrap()
                    .gap(px(10.))
                    .children(ThemePreference::ALL.into_iter().map(|choice| {
                        let selected = settings.choice == choice;
                        let (label, symbol) = match choice {
                            ThemePreference::System => (Text::SystemAppearance, Icon::Monitor),
                            ThemePreference::Explicit(Theme::Light) => {
                                (Text::LightAppearance, Icon::Sun)
                            }
                            ThemePreference::Explicit(Theme::Dark) => {
                                (Text::DarkAppearance, Icon::Moon)
                            }
                        };
                        Self::button(
                            (gpui::ElementId::from("appearance"), choice.code()),
                            locale.text(label),
                            if selected { Icon::Check } else { symbol },
                            if selected {
                                ButtonStyle::Tinted(Tone::Mint)
                            } else {
                                ButtonStyle::Secondary
                            },
                            Action::SetAppearance(choice),
                            cx,
                        )
                    })),
            )
            .when(settings.persistence != Persistence::Saved, |card| {
                let message = match settings.persistence {
                    Persistence::LoadFailed => Text::AppearanceLoadFailed,
                    _ => Text::AppearanceSaveFailed,
                };
                card.child(
                    theme
                        .caption(locale.text(message))
                        .text_color(Tone::Rose.text(theme)),
                )
                .child(Self::button(
                    "retry-appearance",
                    locale.text(Text::RetrySave),
                    Icon::Settings,
                    ButtonStyle::Secondary,
                    Action::SetAppearance(settings.choice),
                    cx,
                ))
            })
    }
}
