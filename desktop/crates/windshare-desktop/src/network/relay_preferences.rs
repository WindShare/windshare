use super::MAX_ACTIVE_RELAYS;
use crate::{
    desktop::Desktop,
    live::{PREVIEW_DEFAULT_FRONTEND, Runtime},
    localization::{Text, language},
    model::Overlay,
};
use gpui::{
    Context, Div, KeyDownEvent, PromptButton, PromptLevel, Role, Toggled, Window, prelude::*, px,
};
use windshare_host::{PreferenceEdit, RelayMode, RelayPreferences, SavedRelay};
use windshare_ui::{Button, ButtonStyle, Icon, Theme, Tone, column, icon, row};

const CONFIRM_RELAY_ACTION: usize = 1;

pub(super) fn prompt_reauthorize_relay(
    relay_url: &str,
    window: &mut Window,
    cx: &mut gpui::App,
) -> impl std::future::Future<Output = bool> + 'static {
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
    async move { answer.await == Ok(CONFIRM_RELAY_ACTION) }
}

pub(super) fn prompt_remove_relay(
    relay_url: &str,
    window: &mut Window,
    cx: &mut gpui::App,
) -> impl std::future::Future<Output = bool> + 'static {
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
    async move { answer.await == Ok(CONFIRM_RELAY_ACTION) }
}

pub(super) fn relay_actions_toggle<V: 'static>(
    index: usize,
    expanded: bool,
    busy: bool,
    on_toggle: impl Fn(&mut V, &mut Context<V>) + 'static,
    cx: &Context<V>,
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
        .on_press(cx.listener(move |this, (), _, cx| on_toggle(this, cx)))
}

#[allow(clippy::too_many_arguments)]
pub(super) fn relay_actions_drawer<V: 'static>(
    theme: Theme,
    relay: &SavedRelay,
    index: usize,
    busy: bool,
    is_preview: bool,
    on_reauthorize: impl Fn(&mut V, &mut Window, &mut Context<V>) + 'static,
    on_remove: impl Fn(&mut V, &mut Window, &mut Context<V>) + 'static,
    cx: &Context<V>,
) -> Div {
    let locale = language(cx);
    let mut actions = row().gap(px(8.)).flex_wrap().justify_end();
    if let Some(console_url) = relay.console_url.clone() {
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
                .disabled(busy || is_preview)
                .on_press(cx.listener(move |this, (), window, cx| {
                    on_reauthorize(this, window, cx);
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
                    on_remove(this, window, cx);
                })),
        )
        .px(px(12.))
        .py(px(8.))
        .rounded(px(10.))
        .bg(theme.inset())
}

pub(super) fn relay_mode_option<V: 'static>(
    theme: Theme,
    prefs: &RelayPreferences,
    mode: RelayMode,
    busy: bool,
    fill_width: bool,
    on_select: impl Fn(&mut V, &mut Context<V>) + Clone + 'static,
    cx: &Context<V>,
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
    let on_key_select = on_select.clone();
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
                        on_select(this, cx);
                    }
                }))
                .on_key_down(cx.listener(move |this, event: &KeyDownEvent, _, cx| {
                    if matches!(event.keystroke.key.as_str(), "space" | "enter") {
                        cx.stop_propagation();
                        if !selected {
                            on_key_select(this, cx);
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

impl Desktop {
    pub(super) fn apply_mode(&mut self, mode: RelayMode, cx: &mut Context<Self>) {
        if matches!(self.runtime, Runtime::Preview(_)) {
            if let Runtime::Preview(preview) = &mut self.runtime {
                preview.preferences.mode = mode;
            }
            cx.notify();
            return;
        }
        if self.network_settings_busy(cx) {
            return;
        }
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        let host = session.host.clone();
        self.network_ui.inline_request_id = host.update(cx, |host, cx| {
            host.submit_preference("", PreferenceEdit::Mode(mode), cx)
        });
        self.network_ui.remove_toast_id = None;
        cx.notify();
    }

    pub(super) fn apply_preference(&mut self, edit: PreferenceEdit, cx: &mut Context<Self>) {
        if matches!(self.runtime, Runtime::Preview(_)) {
            self.apply_preview_preference(edit, cx);
            return;
        }
        if self.network_settings_busy(cx) {
            return;
        }
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        let is_remove = matches!(edit, PreferenceEdit::Remove(_));
        let host = session.host.clone();
        let id = host.update(cx, |host, cx| host.submit_preference("", edit, cx));
        self.network_ui.remove_toast_id = is_remove.then(|| id.clone()).flatten();
        self.network_ui.inline_request_id = id;
        cx.notify();
    }

    pub(super) fn apply_preview_preference(
        &mut self,
        edit: PreferenceEdit,
        cx: &mut Context<Self>,
    ) {
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
                if self.network_ui.expanded_relay.as_deref() == Some(url.as_str()) {
                    self.network_ui.expanded_relay = None;
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
        let updated_prefs = prefs.clone();
        if let Some(Overlay::SharingSetup(_, dialog)) =
            self.overlays.last().map(|frame| &frame.content)
        {
            dialog.update(cx, |dialog, cx| {
                dialog.set_preview_preferences(updated_prefs, cx);
            });
        }
        if let Some(message) = toast {
            self.notify(message, cx);
        }
        cx.notify();
    }

    pub(super) fn confirm_reauthorize_relay(
        &self,
        relay_url: String,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if self.network_settings_busy(cx) || self.runtime.is_preview() {
            return;
        }
        let epoch = self.interaction_epoch();
        let confirmed = prompt_reauthorize_relay(&relay_url, window, cx);
        cx.spawn_in(window, async move |view, cx| {
            if !confirmed.await {
                return;
            }
            let _ = view.update_in(cx, |this, window, cx| {
                if this.interaction_epoch() != epoch
                    || this.network_settings_busy(cx)
                    || !this
                        .relay_preferences_view(cx)
                        .relays
                        .iter()
                        .any(|relay| relay.url == relay_url)
                {
                    return;
                }
                this.network_ui.expanded_relay = None;
                tracing::info!(
                    event = "desktop.relay_reauthorize_confirmed",
                    session = std::process::id()
                );
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
        if self.network_settings_busy(cx) {
            return;
        }
        let epoch = self.interaction_epoch();
        let confirmed = prompt_remove_relay(&relay_url, window, cx);
        cx.spawn_in(window, async move |view, cx| {
            if !confirmed.await {
                return;
            }
            let _ = view.update_in(cx, |this, _, cx| {
                if this.interaction_epoch() != epoch
                    || this.network_settings_busy(cx)
                    || !this
                        .relay_preferences_view(cx)
                        .relays
                        .iter()
                        .any(|relay| relay.url == relay_url)
                {
                    return;
                }
                this.network_ui.expanded_relay = None;
                tracing::info!(
                    event = "desktop.relay_remove_confirmed",
                    session = std::process::id()
                );
                this.apply_preference(PreferenceEdit::Remove(relay_url), cx);
            });
        })
        .detach();
    }
}
