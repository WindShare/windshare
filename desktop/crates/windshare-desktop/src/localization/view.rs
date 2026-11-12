use super::{LanguageChoice, LanguageSettings, Text, language};
use crate::preferences::{Persistence, PreferenceChoice};
use crate::{desktop::Desktop, model::Action};
use gpui::{Div, prelude::*, px};
use windshare_ui::{ButtonStyle, Icon, Theme, Tone, column, row};

impl Desktop {
    pub fn language_card(theme: Theme, cx: &Context<Self>) -> Div {
        let settings = &cx.global::<LanguageSettings>().preference;
        let locale = language(cx);
        theme
            .card()
            .p(px(28.))
            .gap(px(18.))
            .child(
                row()
                    .gap(px(18.))
                    .child(theme.icon_tile(Icon::Globe, Tone::Mint, 44.))
                    .child(
                        column()
                            .flex_1()
                            .gap(px(7.))
                            .child(theme.heading(locale.text(Text::Language), 22.))
                            .child(theme.caption(locale.text(Text::LanguageDescription))),
                    ),
            )
            .child(
                row()
                    .flex_wrap()
                    .gap(px(10.))
                    .children(LanguageChoice::ALL.into_iter().map(|choice| {
                        let selected = settings.choice == choice;
                        let label = match choice {
                            LanguageChoice::System => locale.text(Text::SystemLanguage),
                            LanguageChoice::Language(language) => language.native_name(),
                        };
                        Self::button(
                            choice.code(),
                            label,
                            if selected { Icon::Check } else { Icon::Globe },
                            if selected {
                                ButtonStyle::Tinted(Tone::Mint)
                            } else {
                                ButtonStyle::Secondary
                            },
                            Action::SetLanguage(choice),
                            cx,
                        )
                    })),
            )
            .when(settings.persistence != Persistence::Saved, |card| {
                let message = if settings.persistence == Persistence::LoadFailed {
                    Text::LanguageLoadFailed
                } else {
                    Text::LanguageSaveFailed
                };
                card.child(
                    column()
                        .gap(px(12.))
                        .child(
                            theme
                                .caption(locale.text(message))
                                .text_color(Tone::Rose.text(theme)),
                        )
                        .child(Self::button(
                            "retry-language",
                            locale.text(Text::RetrySave),
                            Icon::Settings,
                            ButtonStyle::Secondary,
                            Action::SetLanguage(settings.choice),
                            cx,
                        )),
                )
            })
    }
}
