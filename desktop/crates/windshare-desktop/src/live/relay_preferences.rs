use super::{Runtime, SetupPage, session::PREVIEW_DEFAULT_FRONTEND};
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::{Action, Overlay, SharingSetupTarget},
};
use gpui::{
    Div, ElementId, KeyDownEvent, PromptButton, PromptLevel, Role, Toggled, Window, prelude::*, px,
};
use windshare_host::{PreferenceEdit, RelayMode, RelayOperation, RelayPreferences, SavedRelay};
use windshare_ui::{Button, ButtonStyle, Icon, Theme, Tone, column, icon, row};

pub(super) const MAX_ACTIVE_RELAYS: usize = 8;
const CONFIRM_RELAY_ACTION: usize = 1;

impl Desktop {
    pub(super) fn apply_preference(&mut self, edit: PreferenceEdit, cx: &mut Context<Self>) {
        if matches!(self.runtime, Runtime::Preview(_)) {
            self.apply_preview_preference(edit, cx);
            return;
        }
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        if self.setup_form.pending.is_some()
            || session.model.setup.unfinished()
            || self.runtime.selection_disabled()
        {
            return;
        }
        let operation = match self.overlays.last().map(|frame| &frame.content) {
            Some(Overlay::SharingSetup(target)) => match target.share_id() {
                Some(id) => {
                    let Some(op) = self.native_operation(id) else {
                        return;
                    };
                    op
                }
                None => "",
            },
            _ => "",
        };
        let pending_toast = matches!(edit, PreferenceEdit::Remove(_)).then_some(Text::RelayRemoved);
        self.setup_form.pending = session.controller.edit_preference(operation, edit);
        self.setup_form.pending_toast = self.setup_form.pending.as_ref().and(pending_toast);
        self.setup_form.feedback = None;
        cx.notify();
    }

    fn apply_preview_preference(&mut self, edit: PreferenceEdit, cx: &mut Context<Self>) {
        let Runtime::Preview(preview) = &mut self.runtime else {
            return;
        };
        let prefs = &mut preview.preferences;
        let mut toast = None;
        match edit {
            PreferenceEdit::Mode(mode) => prefs.mode = mode,
            PreferenceEdit::Enable(url) => {
                if !prefs.at_capacity
                    && let Some(relay) = prefs.relays.iter_mut().find(|r| r.url == url)
                {
                    relay.enabled = true;
                }
            }
            PreferenceEdit::Disable(url) => {
                if let Some(relay) = prefs.relays.iter_mut().find(|r| r.url == url) {
                    relay.enabled = false;
                }
            }
            PreferenceEdit::Remove(url) => {
                prefs.relays.retain(|relay| relay.url != url);
                if self.setup_form.expanded_relay.as_deref() == Some(url.as_str()) {
                    self.setup_form.expanded_relay = None;
                }
                toast = Some(Text::RelayRemoved);
            }
            PreferenceEdit::Frontend(url) => {
                if url.is_empty() {
                    prefs.frontend = PREVIEW_DEFAULT_FRONTEND.into();
                    prefs.custom_frontend = false;
                } else {
                    prefs.frontend = url;
                    prefs.custom_frontend = true;
                }
                toast = Some(Text::FrontendSaved);
            }
        }
        prefs.at_capacity =
            prefs.relays.iter().filter(|relay| relay.enabled).count() >= MAX_ACTIVE_RELAYS;
        if let Some(message) = toast {
            self.notify(message, cx);
        }
        cx.notify();
    }

    pub(super) fn toggle_relay_actions(&mut self, relay_url: String, cx: &mut Context<Self>) {
        self.setup_form.expanded_relay =
            if self.setup_form.expanded_relay.as_deref() == Some(relay_url.as_str()) {
                None
            } else {
                Some(relay_url)
            };
        cx.notify();
    }

    pub(super) fn confirm_reauthorize_relay(
        &self,
        relay_url: String,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if self.network_settings_busy() || self.runtime.is_preview() {
            return;
        }
        let epoch = self.interaction_epoch();
        let locale = language(cx);
        let detail = format!(
            "{relay_url}\n\n{}",
            locale.text(Text::ReauthorizeDeviceDetail)
        );
        let answer = window.prompt(
            PromptLevel::Info,
            locale.text(Text::ReauthorizeDeviceTitle),
            Some(&detail),
            &[
                PromptButton::Cancel(locale.text(Text::Cancel).into()),
                PromptButton::Other(locale.text(Text::ReauthorizeDevice).into()),
            ],
            cx,
        );
        cx.spawn_in(window, async move |view, cx| {
            if answer.await != Ok(CONFIRM_RELAY_ACTION) {
                return;
            }
            let _ = view.update_in(cx, |this, window, cx| {
                if this.interaction_epoch() != epoch
                    || this.network_settings_busy()
                    || !this
                        .relay_preferences_view()
                        .relays
                        .iter()
                        .any(|relay| relay.url == relay_url)
                {
                    return;
                }
                this.setup_form.expanded_relay = None;
                tracing::info!(
                    event = "desktop.relay_reauthorize_confirmed",
                    session = std::process::id()
                );
                if !this
                    .overlays
                    .last()
                    .is_some_and(|frame| matches!(frame.content, Overlay::SharingSetup(_)))
                {
                    this.open_overlay(
                        Overlay::SharingSetup(SharingSetupTarget::Defaults),
                        window,
                        cx,
                    );
                }
                this.authorize_relay(relay_url, window, cx);
            });
        })
        .detach();
    }

    pub(super) fn confirm_remove_relay(
        &self,
        relay_url: String,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if self.network_settings_busy() {
            return;
        }
        let epoch = self.interaction_epoch();
        let locale = language(cx);
        let detail = format!("{relay_url}\n\n{}", locale.text(Text::RemoveRelayDetail));
        let answer = window.prompt(
            PromptLevel::Warning,
            locale.text(Text::RemoveRelayTitle),
            Some(&detail),
            &[
                PromptButton::Cancel(locale.text(Text::Cancel).into()),
                PromptButton::Other(locale.text(Text::Remove).into()),
            ],
            cx,
        );
        cx.spawn_in(window, async move |view, cx| {
            if answer.await != Ok(CONFIRM_RELAY_ACTION) {
                return;
            }
            let _ = view.update_in(cx, |this, _, cx| {
                if this.interaction_epoch() != epoch
                    || this.network_settings_busy()
                    || !this
                        .relay_preferences_view()
                        .relays
                        .iter()
                        .any(|relay| relay.url == relay_url)
                {
                    return;
                }
                this.setup_form.expanded_relay = None;
                tracing::info!(
                    event = "desktop.relay_remove_confirmed",
                    session = std::process::id()
                );
                this.apply_preference(PreferenceEdit::Remove(relay_url), cx);
            });
        })
        .detach();
    }

    pub(super) fn relay_actions_toggle(
        index: usize,
        relay_url: String,
        expanded: bool,
        busy: bool,
        cx: &Context<Self>,
    ) -> Button {
        Button::new(("relay-actions-toggle", index), "")
            .icon(Icon::More)
            .compact()
            .style(if expanded {
                ButtonStyle::Secondary
            } else {
                ButtonStyle::Ghost
            })
            .tooltip(language(cx).text(Text::RelayActions))
            .disabled(busy)
            .on_press(cx.listener(move |this, (), _, cx| {
                this.toggle_relay_actions(relay_url.clone(), cx);
            }))
    }

    pub(super) fn relay_actions_drawer(
        &self,
        theme: Theme,
        relay: &SavedRelay,
        index: usize,
        busy: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let remove_url = relay.url.clone();
        let mut actions = row().gap(px(8.)).flex_wrap().justify_end();
        if let Some(console_url) = relay.console_url.clone() {
            let reauth_url = relay.url.clone();
            actions = actions
                .child(
                    Button::new(("relay-console", index), locale.text(Text::ManageRelay))
                        .icon(Icon::External)
                        .compact()
                        .style(ButtonStyle::Secondary)
                        .on_press(move |(), _, cx| cx.open_url(&console_url)),
                )
                .child(
                    Button::new(
                        ("reauthorize-relay", index),
                        locale.text(Text::ReauthorizeDevice),
                    )
                    .icon(Icon::Shield)
                    .compact()
                    .style(ButtonStyle::Secondary)
                    .disabled(busy || self.runtime.is_preview())
                    .on_press(cx.listener(move |this, (), window, cx| {
                        this.confirm_reauthorize_relay(reauth_url.clone(), window, cx);
                    })),
                );
        }
        actions
            .child(
                Button::new(("remove-relay", index), locale.text(Text::RemoveRelay))
                    .icon(Icon::Trash)
                    .compact()
                    .style(ButtonStyle::Tinted(Tone::Rose))
                    .disabled(busy)
                    .on_press(cx.listener(move |this, (), window, cx| {
                        this.confirm_remove_relay(remove_url.clone(), window, cx);
                    })),
            )
            .px(px(12.))
            .py(px(8.))
            .rounded(px(10.))
            .bg(theme.inset())
    }

    pub(super) fn preference_button(
        id: impl Into<ElementId>,
        label: &'static str,
        symbol: Icon,
        edit: PreferenceEdit,
        cx: &Context<Self>,
    ) -> Button {
        Button::new(id, label)
            .icon(symbol)
            .compact()
            .on_press(cx.listener(move |this, (), _, cx| {
                this.apply_preference(edit.clone(), cx);
            }))
    }

    pub(super) fn relay_preferences(
        &self,
        theme: Theme,
        prefs: &RelayPreferences,
        switch_share: Option<usize>,
        busy: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let enabled = prefs.relays.iter().any(|relay| relay.enabled);
        let mut list = column().gap(px(12.)).child(
            row()
                .justify_between()
                .gap(px(12.))
                .child(theme.heading(locale.text(Text::CustomRelays), 17.))
                .child(
                    Self::button(
                        "add-relay",
                        locale.text(Text::AddRelay),
                        Icon::Plus,
                        ButtonStyle::Secondary,
                        Action::SetupPage(SetupPage::Relay(RelayOperation::AddProfile)),
                        cx,
                    )
                    .compact()
                    .disabled(busy),
                ),
        );
        if prefs.relays.is_empty() && (switch_share.is_some() || prefs.mode == RelayMode::Automatic)
        {
            list = list.child(theme.caption(locale.text(Text::NoCustomRelays)));
        }
        if switch_share.is_none() && !enabled && prefs.mode == RelayMode::Selected {
            list = list.child(
                theme
                    .caption(locale.text(Text::NoEnabledRelays))
                    .text_color(Tone::Peach.text(theme)),
            );
        }
        if switch_share.is_none() && prefs.at_capacity {
            list = list.child(theme.caption(locale.text(Text::RelaySlotsFull)));
        }
        list = list.children(prefs.relays.iter().enumerate().map(|(index, relay)| {
            self.saved_relay(
                theme,
                relay,
                index,
                switch_share,
                busy,
                prefs.at_capacity,
                cx,
            )
        }));
        if switch_share.is_some() {
            return list;
        }
        column()
            .gap(px(20.))
            .child(
                column()
                    .id("relay-modes")
                    .role(Role::RadioGroup)
                    .aria_label(locale.text(Text::RelayChoice))
                    .gap(px(8.))
                    .child(Self::relay_mode_option(
                        theme,
                        prefs,
                        RelayMode::Automatic,
                        busy,
                        false,
                        cx,
                    ))
                    .child(Self::relay_mode_option(
                        theme,
                        prefs,
                        RelayMode::Selected,
                        busy,
                        false,
                        cx,
                    )),
            )
            .child(list)
            .child(theme.separator())
            .child(
                row()
                    .justify_between()
                    .gap(px(12.))
                    .child(
                        column()
                            .flex_1()
                            .min_w_0()
                            .gap(px(4.))
                            .child(theme.heading(locale.text(Text::ReceiverSite), 15.))
                            .child(theme.caption(prefs.frontend.clone()).line_clamp(2)),
                    )
                    .child(
                        Self::button(
                            "edit-receiver-site",
                            locale.text(Text::ChangeWebsite),
                            Icon::Link,
                            ButtonStyle::Quiet,
                            Action::SetupPage(SetupPage::ReceiverSite),
                            cx,
                        )
                        .compact()
                        .disabled(busy),
                    ),
            )
    }

    #[allow(clippy::too_many_arguments)]
    fn saved_relay(
        &self,
        theme: Theme,
        relay: &SavedRelay,
        index: usize,
        switch_share: Option<usize>,
        busy: bool,
        at_capacity: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let expanded = switch_share.is_none()
            && self.setup_form.expanded_relay.as_deref() == Some(relay.url.as_str());
        let choice = switch_share.map_or_else(
            || {
                let (label, symbol, edit) = if relay.enabled {
                    (
                        Text::DisableRelay,
                        Icon::Pause,
                        PreferenceEdit::Disable(relay.url.clone()),
                    )
                } else {
                    (
                        Text::EnableRelay,
                        Icon::Play,
                        PreferenceEdit::Enable(relay.url.clone()),
                    )
                };
                Self::preference_button(
                    ("relay-toggle", index),
                    locale.text(label),
                    symbol,
                    edit,
                    cx,
                )
                .disabled(busy || !relay.enabled && at_capacity)
            },
            |id| {
                let url = relay.url.clone();
                Button::new(("use-relay", index), locale.text(Text::UseRelayForShare))
                    .icon(Icon::Arrow)
                    .compact()
                    .style(ButtonStyle::Secondary)
                    .disabled(busy)
                    .on_press(cx.listener(move |this, (), window, cx| {
                        this.switch_share_relay(id, &url, window, cx);
                    }))
            },
        );
        let actions = row()
            .gap(px(6.))
            .flex_wrap()
            .justify_end()
            .when(switch_share.is_none(), |actions| {
                actions.child(Self::relay_actions_toggle(
                    index,
                    relay.url.clone(),
                    expanded,
                    busy,
                    cx,
                ))
            })
            .child(choice);
        column()
            .gap(px(10.))
            .pt(px(10.))
            .border_t_1()
            .border_color(theme.border())
            .child(
                row()
                    .gap(px(10.))
                    .when(switch_share.is_none(), |row| {
                        row.child(theme.dot(if relay.enabled {
                            Tone::Mint
                        } else {
                            Tone::Neutral
                        }))
                    })
                    .child(
                        theme
                            .label(relay.url.clone(), 14.)
                            .flex_1()
                            .min_w_0()
                            .line_clamp(2),
                    )
                    .child(actions),
            )
            .when(expanded, |col| {
                col.child(self.relay_actions_drawer(theme, relay, index, busy, cx))
            })
    }

    pub(super) fn relay_mode_option(
        theme: Theme,
        prefs: &RelayPreferences,
        mode: RelayMode,
        busy: bool,
        fill_width: bool,
        cx: &Context<Self>,
    ) -> impl IntoElement {
        let locale = language(cx);
        let (label, description) = match mode {
            RelayMode::Automatic => (
                Text::AutomaticRelays,
                if prefs.relays.is_empty() {
                    Text::AutomaticRelayDetail
                } else {
                    Text::AutomaticWithCustom
                },
            ),
            RelayMode::Selected => (Text::SelectedRelays, Text::SelectedRelayDetail),
        };
        let selected = prefs.mode == mode;
        let accent = Tone::Mint.text(theme);
        let indicator = row()
            .size(px(18.))
            .flex_shrink_0()
            .when(fill_width, |el| el.mt(px(3.)))
            .items_center()
            .justify_center()
            .rounded_full()
            .border_1()
            .border_color(if selected { accent } else { theme.muted_text() })
            .when(selected, |el| {
                el.bg(accent)
                    .child(icon(Icon::Check, 12.).text_color(theme.surface()))
            });
        row()
            .id(("relay-choice", mode as usize))
            .role(Role::RadioButton)
            .aria_label(format!(
                "{} {}",
                locale.text(label),
                locale.text(description)
            ))
            .aria_toggled(if selected {
                Toggled::True
            } else {
                Toggled::False
            })
            .gap(px(14.))
            .when(fill_width, |el| el.flex_1().min_w_0().items_start())
            .px(px(16.))
            .py(px(14.))
            .rounded(px(12.))
            .border_1()
            .border_color(if selected {
                accent.opacity(0.55)
            } else {
                theme.border()
            })
            .when(selected, |el| el.bg(Tone::Mint.surface(theme)))
            .when(!busy, |el| {
                el.focusable()
                    .tab_index(0)
                    .cursor_pointer()
                    .hover(|style| style.border_color(accent))
                    .focus_visible(|style| style.border_color(theme.focus_ring()).border_2())
                    .on_click(cx.listener(move |this, _, _, cx| {
                        if !selected {
                            this.apply_preference(PreferenceEdit::Mode(mode), cx);
                        }
                    }))
                    .on_key_down(cx.listener(move |this, event: &KeyDownEvent, _, cx| {
                        if matches!(event.keystroke.key.as_str(), "space" | "enter") {
                            cx.stop_propagation();
                            if !selected {
                                this.apply_preference(PreferenceEdit::Mode(mode), cx);
                            }
                        }
                    }))
            })
            .child(indicator)
            .child(
                column()
                    .min_w_0()
                    .flex_1()
                    .gap(px(3.))
                    .child(theme.heading(locale.text(label), 16.))
                    .child(theme.caption(locale.text(description))),
            )
    }
}
