use super::Runtime;
use crate::{
    desktop::Desktop,
    localization::{Language, Text, language},
    model::Action,
};
use gpui::{Div, prelude::*, px};
use windshare_host::{RelayMode, RelayPreferences};
use windshare_ui::{Button, ButtonStyle, Icon, Theme, Tone, column, row};

fn relay_summary(prefs: &RelayPreferences, locale: Language) -> String {
    let count = prefs.relays.iter().filter(|relay| relay.enabled).count();
    match (prefs.mode, count) {
        (RelayMode::Automatic, 0) => locale.text(Text::AutomaticRelayDetail).into(),
        (RelayMode::Selected, 0) => locale.text(Text::NoEnabledRelays).into(),
        (mode, _) => match locale {
            Language::English => {
                let noun = if count == 1 { "relay" } else { "relays" };
                if mode == RelayMode::Automatic && !prefs.at_capacity {
                    format!("{count} custom {noun} plus relays selected by WindShare")
                } else if mode == RelayMode::Automatic {
                    format!("Automatic · All {count} relay slots in use")
                } else {
                    format!("Custom relays only · {count} enabled")
                }
            }
            Language::SimplifiedChinese => {
                if mode == RelayMode::Automatic && !prefs.at_capacity {
                    format!("优先使用 {count} 台自定义中转，其余由 WindShare 自动补充。")
                } else if mode == RelayMode::Automatic {
                    format!("自动选择 · {count} 个中转名额均已占用")
                } else {
                    format!("仅用自定义中转 · 已启用 {count} 台")
                }
            }
        },
    }
}

pub(super) fn setting_row(theme: Theme, title: &str, detail: String, action: Button) -> Div {
    row()
        .gap(px(18.))
        .child(
            column()
                .flex_1()
                .min_w_0()
                .gap(px(5.))
                .child(theme.heading(title.to_owned(), 16.))
                .child(theme.caption(detail)),
        )
        .child(action.compact())
}

impl Desktop {
    pub fn network_settings(&self, theme: Theme, cx: &Context<Self>) -> Option<Div> {
        let Runtime::Native(session) = &self.runtime else {
            return None;
        };
        let locale = language(cx);
        let mut card = theme
            .card()
            .p(px(28.))
            .gap(px(20.))
            .child(
                row()
                    .gap(px(14.))
                    .child(theme.icon_tile(Icon::Globe, Tone::Mint, 44.))
                    .child(theme.heading(locale.text(Text::SharingNetwork), 22.)),
            )
            .child(setting_row(
                theme,
                locale.text(Text::RelayChoice),
                relay_summary(&session.model.preferences, locale),
                Self::button(
                    "network-settings",
                    locale.text(Text::ChooseRelays),
                    Icon::Sliders,
                    ButtonStyle::Secondary,
                    Action::ConfigureNetwork,
                    cx,
                ),
            ));
        if let Some(account) = self.account_setting(theme, cx) {
            card = card.child(theme.separator()).child(account);
        }
        Some(card)
    }
}
