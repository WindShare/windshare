use super::{Runtime, SetupPage};
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::{Action, Overlay},
};
use gpui::{Div, ElementId, KeyDownEvent, Role, Toggled, prelude::*, px};
use windshare_host::{PreferenceEdit, RelayMode, RelayOperation, RelayPreferences};
use windshare_ui::{Button, ButtonStyle, Icon, Theme, Tone, column, icon, row};

impl Desktop {
    pub(super) fn apply_preference(&mut self, edit: PreferenceEdit, cx: &mut Context<Self>) {
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        let Some(Overlay::SharingSetup(share_id)) =
            self.overlays.last().map(|frame| &frame.content)
        else {
            return;
        };
        if self.setup_form.pending.is_some()
            || session.model.setup.unfinished()
            || self.runtime.selection_disabled()
        {
            return;
        }
        let operation = match share_id {
            Some(id) => {
                let Some(op) = self.native_operation(*id) else {
                    return;
                };
                op
            }
            None => "",
        };
        self.setup_form.pending = session.controller.edit_preference(operation, edit);
        self.setup_form.feedback = None;
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
        if prefs.relays.is_empty() && prefs.mode == RelayMode::Automatic {
            list = list.child(theme.caption(locale.text(Text::NoCustomRelays)));
        }
        if !enabled && prefs.mode == RelayMode::Selected {
            list = list.child(
                theme
                    .caption(locale.text(Text::NoEnabledRelays))
                    .text_color(Tone::Peach.text(theme)),
            );
        }
        if prefs.at_capacity {
            list = list.child(theme.caption(locale.text(Text::RelaySlotsFull)));
        }
        list = list.children(prefs.relays.iter().enumerate().map(|(index, relay)| {
            Self::saved_relay(theme, relay, index, busy, prefs.at_capacity, cx)
        }));
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
                        cx,
                    ))
                    .child(Self::relay_mode_option(
                        theme,
                        prefs,
                        RelayMode::Selected,
                        busy,
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
        busy: bool,
        at_capacity: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
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
            .child(theme.dot(if relay.enabled {
                Tone::Mint
            } else {
                Tone::Neutral
            }))
            .child(
                theme
                    .label(relay.url.clone(), 14.)
                    .flex_1()
                    .min_w_0()
                    .line_clamp(2),
            )
            .child(
                actions.child(
                    Self::preference_button(
                        ("relay-toggle", index),
                        locale.text(label),
                        symbol,
                        edit,
                        cx,
                    )
                    .disabled(busy || !relay.enabled && at_capacity),
                ),
            )
    }

    fn relay_mode_option(
        theme: Theme,
        prefs: &RelayPreferences,
        mode: RelayMode,
        busy: bool,
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
            .px(px(16.))
            .py(px(12.))
            .rounded(px(10.))
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
