use super::{Persistence, PreferenceChoice, ShareAnimationSettings};
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::Action,
    motion::ShareAnimation,
};
use gpui::{Div, prelude::*, px};
use windshare_ui::{ButtonStyle, Icon, Theme, Tone, column, row};

impl Desktop {
    pub fn share_animation_setting(theme: Theme, cx: &Context<Self>) -> Div {
        let settings = cx.global::<ShareAnimationSettings>();
        let locale = language(cx);
        column()
            .py(px(18.))
            .gap(px(12.))
            .child(
                row()
                    .flex_wrap()
                    .gap(px(18.))
                    .child(theme.icon_tile(Icon::Spark, Tone::Neutral, 44.))
                    .child(
                        theme
                            .heading(locale.text(Text::ShareAnimation), 17.)
                            .flex_1(),
                    )
                    .child(
                        row()
                            .gap(px(10.))
                            .children(ShareAnimation::ALL.into_iter().map(|choice| {
                                let selected = settings.choice == choice;
                                let (label, symbol) = match choice {
                                    ShareAnimation::Whirl => (Text::WhirlAnimation, Icon::Wind),
                                    ShareAnimation::Paper => (Text::PaperAnimation, Icon::File),
                                };
                                Self::button(
                                    (gpui::ElementId::from("share-animation"), choice.code()),
                                    locale.text(label),
                                    if selected { Icon::Check } else { symbol },
                                    if selected {
                                        ButtonStyle::Tinted(Tone::Mint)
                                    } else {
                                        ButtonStyle::Secondary
                                    },
                                    Action::SetShareAnimation(choice),
                                    cx,
                                )
                                .compact()
                            })),
                    ),
            )
            .when(settings.persistence != Persistence::Saved, |el| {
                let message = match settings.persistence {
                    Persistence::LoadFailed => Text::ShareAnimationLoadFailed,
                    _ => Text::ShareAnimationSaveFailed,
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
                                "retry-share-animation",
                                locale.text(Text::RetrySave),
                                Icon::Settings,
                                ButtonStyle::Secondary,
                                Action::SetShareAnimation(settings.choice),
                                cx,
                            )
                            .compact(),
                        ),
                )
            })
    }
}
