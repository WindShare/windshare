use super::Runtime;
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::Action,
};
use gpui::{Div, prelude::*, px};
use windshare_host::PreferenceEdit;
use windshare_ui::{ButtonStyle, Icon, Theme, column, row};

#[derive(Clone, Copy, Debug)]
pub enum RelayAction {
    Mode(bool),
    Toggle(usize),
    Manage(usize),
    UseFrontend(usize),
    DefaultFrontend,
}

impl Desktop {
    pub fn relay_action(&self, action: RelayAction, cx: &Context<Self>) {
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        let relays = &session.model.preferences.relays;
        let edit = match action {
            RelayAction::Mode(selected) => {
                PreferenceEdit::Mode(if selected { "selected" } else { "automatic" }.into())
            }
            RelayAction::Toggle(i) => {
                let Some(relay) = relays.get(i) else { return };
                if relay.enabled {
                    PreferenceEdit::Disable(relay.url.clone())
                } else {
                    PreferenceEdit::Enable(relay.url.clone())
                }
            }
            RelayAction::Manage(i) => {
                if let Some(url) = relays.get(i).and_then(|r| r.console_url.as_deref()) {
                    cx.open_url(url);
                }
                return;
            }
            RelayAction::DefaultFrontend => PreferenceEdit::Frontend(String::new()),
            RelayAction::UseFrontend(i) => {
                let Some(url) = relays.get(i).and_then(|r| r.offered_frontend.clone()) else {
                    return;
                };
                PreferenceEdit::Frontend(url)
            }
        };
        session.controller.edit_preference(edit);
    }

    pub(super) fn relay_preferences(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let Runtime::Native(session) = &self.runtime else {
            return column();
        };
        let prefs = &session.model.preferences;
        let locale = language(cx);
        let busy = session.model.setup.unfinished();
        let selected = prefs.mode == "selected";
        let mut view = column()
            .gap(px(8.))
            .child(Self::relay_mode(selected, busy, cx))
            .child(theme.caption(format!(
                "{} · {}",
                locale.text(Text::FrontendAddress),
                prefs.frontend
            )));
        if prefs.custom_frontend {
            view = view.child(
                Self::button(
                    "default-receiver",
                    locale.text(Text::DefaultReceiver),
                    Icon::Link,
                    ButtonStyle::Quiet,
                    Action::Relay(RelayAction::DefaultFrontend),
                    cx,
                )
                .disabled(busy),
            );
        }
        if !selected && prefs.at_capacity {
            view = view.child(theme.caption(locale.text(Text::RelaySlotsFull)));
        }
        for (index, relay) in prefs.relays.iter().enumerate() {
            let mut actions = row().gap(px(8.)).flex_wrap().child(
                Self::button(
                    ("relay-enabled", index),
                    locale.text(if relay.enabled {
                        Text::RelayEnabled
                    } else {
                        Text::RelayDisabled
                    }),
                    if relay.enabled {
                        Icon::Check
                    } else {
                        Icon::Link
                    },
                    ButtonStyle::Secondary,
                    Action::Relay(RelayAction::Toggle(index)),
                    cx,
                )
                .disabled(busy || !relay.enabled && prefs.at_capacity),
            );
            if relay.console_url.is_some() {
                actions = actions.child(Self::button(
                    ("relay-manage", index),
                    locale.text(Text::ManageRelay),
                    Icon::External,
                    ButtonStyle::Quiet,
                    Action::Relay(RelayAction::Manage(index)),
                    cx,
                ));
            }
            if let Some(url) = relay
                .offered_frontend
                .as_ref()
                .filter(|url| **url != prefs.frontend)
            {
                actions = actions.child(
                    Self::button(
                        ("relay-frontend", index),
                        format!("{} · {url}", locale.text(Text::UseReceiver)),
                        Icon::Link,
                        ButtonStyle::Quiet,
                        Action::Relay(RelayAction::UseFrontend(index)),
                        cx,
                    )
                    .disabled(busy),
                );
            }
            view = view
                .child(theme.label(relay.url.clone(), 14.))
                .child(actions);
        }
        view
    }

    fn relay_mode(selected: bool, busy: bool, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        row().gap(px(8.)).children(
            [(false, Text::AutomaticRelays), (true, Text::SelectedRelays)]
                .into_iter()
                .enumerate()
                .map(|(index, (only, label))| {
                    Self::button(
                        ("relay-mode", index),
                        locale.text(label),
                        Icon::Link,
                        if selected == only {
                            ButtonStyle::Primary
                        } else {
                            ButtonStyle::Secondary
                        },
                        Action::Relay(RelayAction::Mode(only)),
                        cx,
                    )
                    .disabled(busy || selected == only)
                }),
        )
    }
}
