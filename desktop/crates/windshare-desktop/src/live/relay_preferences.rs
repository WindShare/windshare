use super::{Runtime, SetupPage, session::PREVIEW_DEFAULT_FRONTEND};
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::{Action, Overlay},
};
use gpui::{Div, ElementId, KeyDownEvent, Role, Toggled, prelude::*, px};
use windshare_host::{PreferenceEdit, RelayMode, RelayOperation, RelayPreferences};
use windshare_ui::{Button, ButtonStyle, Icon, Theme, Tone, column, icon, row};

pub(super) const MAX_ACTIVE_RELAYS: usize = 8;

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
        self.setup_form.pending = session.controller.edit_preference(operation, edit);
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
            Self::saved_relay(
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

    fn saved_relay(
        theme: Theme,
        relay: &windshare_host::SavedRelay,
        index: usize,
        switch_share: Option<usize>,
        busy: bool,
        at_capacity: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
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
        let mut actions = row().gap(px(6.)).flex_wrap().justify_end();
        if let Some(url) = relay.console_url.clone() {
            let relay_url = relay.url.clone();
            actions = actions
                .child(
                    Button::new(("authorize-relay", index), locale.text(Text::ApproveDevice))
                        .icon(Icon::Shield)
                        .compact()
                        .style(ButtonStyle::Ghost)
                        .disabled(busy)
                        .on_press(cx.listener(move |this, (), window, cx| {
                            this.authorize_relay(relay_url.clone(), window, cx);
                        })),
                )
                .child(
                    Button::new(("relay-console", index), "")
                        .icon(Icon::External)
                        .compact()
                        .style(ButtonStyle::Ghost)
                        .tooltip(locale.text(Text::ManageRelay))
                        .on_press(move |(), _, cx| cx.open_url(&url)),
                );
        }
        row()
            .gap(px(10.))
            .pt(px(10.))
            .border_t_1()
            .border_color(theme.border())
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
            .child(actions.child(choice))
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
