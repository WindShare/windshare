use super::{
    MAX_ACTIVE_RELAYS, SetupPage,
    relay_preferences::{relay_actions_drawer, relay_actions_toggle, relay_mode_option},
};
use crate::{
    desktop::Desktop,
    live::Runtime,
    localization::{Text, language},
    model::Overlay,
};
use gpui::{Context, Div, ElementId, FontWeight, prelude::*, px};
use std::collections::BTreeSet;
use windshare_host::{PreferenceEdit, RelayMode, RelayOperation, RelayPreferences, SavedRelay};
use windshare_ui::{Button, ButtonStyle, FONT, Icon, Theme, Tone, column, row};

const CARD_PADDING_X: f32 = 24.;
const CARD_PADDING_Y: f32 = 20.;
const CARD_ICON_SIZE: f32 = 36.;
const SECTION_GAP: f32 = 16.;

pub fn setting_row(theme: Theme, title: &str, detail: String, action: Button) -> Div {
    row()
        .gap(px(18.))
        .child(
            column()
                .flex_1()
                .min_w_0()
                .gap(px(4.))
                .child(
                    theme
                        .label(title.to_owned(), 15.)
                        .font_weight(FontWeight::MEDIUM),
                )
                .child(theme.caption(detail)),
        )
        .child(action.compact())
}

impl Desktop {
    pub(crate) fn relay_preferences_view<'a>(&'a self, cx: &'a gpui::App) -> &'a RelayPreferences {
        match &self.runtime {
            Runtime::Native(session) => &session.model(cx).preferences,
            Runtime::Preview(preview) => &preview.preferences,
        }
    }

    pub(crate) fn network_attention_tone(&self, cx: &gpui::App) -> Option<Tone> {
        if let Runtime::Native(session) = &self.runtime
            && session.model(cx).setup.unfinished()
        {
            return Some(Tone::Peach);
        }
        let prefs = self.relay_preferences_view(cx);
        let enabled = prefs.relays.iter().any(|relay| relay.enabled);
        (prefs.mode == RelayMode::Selected && !enabled).then_some(Tone::Peach)
    }

    pub(super) fn network_settings_busy(&self, cx: &gpui::App) -> bool {
        let (pending, setup_unfinished) = match &self.runtime {
            Runtime::Native(session) => {
                let model = session.model(cx);
                (model.setup_pending(), model.setup.unfinished())
            }
            Runtime::Preview(_) => (false, false),
        };
        pending || setup_unfinished || self.runtime.selection_disabled(cx)
    }

    pub fn network_settings_page(&self, theme: Theme, compact: bool, cx: &Context<Self>) -> Div {
        let prefs = self.relay_preferences_view(cx);
        let busy = self.network_settings_busy(cx);
        column()
            .gap(px(SECTION_GAP))
            .child(Self::relay_mode_card(theme, prefs, compact, busy, cx))
            .child(self.custom_relays_card(theme, prefs, busy, cx))
            .child(Self::receiver_site_card(theme, prefs, busy, cx))
    }

    fn preference_button(
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

    fn relay_mode_card(
        theme: Theme,
        prefs: &RelayPreferences,
        compact: bool,
        busy: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let options = if compact {
            column().gap(px(10.))
        } else {
            row().items_stretch().gap(px(12.))
        };
        theme
            .card()
            .px(px(CARD_PADDING_X))
            .py(px(CARD_PADDING_Y))
            .gap(px(14.))
            .child(
                row()
                    .gap(px(12.))
                    .child(theme.icon_tile(Icon::Globe, Tone::Mint, CARD_ICON_SIZE))
                    .child(theme.heading(locale.text(Text::RelayChoice), 18.)),
            )
            .child(
                options
                    .id("relay-modes")
                    .role(gpui::Role::RadioGroup)
                    .aria_label(locale.text(Text::RelayChoice))
                    .child(relay_mode_option(
                        theme,
                        prefs,
                        RelayMode::Automatic,
                        busy,
                        !compact,
                        |this, cx| this.apply_mode(RelayMode::Automatic, cx),
                        cx,
                    ))
                    .child(relay_mode_option(
                        theme,
                        prefs,
                        RelayMode::Selected,
                        busy,
                        !compact,
                        |this, cx| this.apply_mode(RelayMode::Selected, cx),
                        cx,
                    )),
            )
    }

    fn custom_relays_card(
        &self,
        theme: Theme,
        prefs: &RelayPreferences,
        busy: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let enabled_count = prefs.relays.iter().filter(|relay| relay.enabled).count();
        let needs_relay = prefs.mode == RelayMode::Selected && enabled_count == 0;
        let slot_tone = if needs_relay || prefs.at_capacity {
            Tone::Peach
        } else if enabled_count > 0 {
            Tone::Mint
        } else {
            Tone::Neutral
        };
        let mut card = theme
            .card()
            .px(px(CARD_PADDING_X))
            .py(px(CARD_PADDING_Y))
            .gap(px(12.))
            .child(
                row()
                    .justify_between()
                    .flex_wrap()
                    .gap(px(12.))
                    .child(
                        row()
                            .gap(px(12.))
                            .child(theme.icon_tile(Icon::Sliders, Tone::Blue, CARD_ICON_SIZE))
                            .child(theme.heading(locale.text(Text::CustomRelays), 18.))
                            .child(theme.badge(
                                locale.relay_slot_usage(enabled_count, MAX_ACTIVE_RELAYS),
                                slot_tone,
                            )),
                    )
                    .child(
                        Button::new("add-relay", locale.text(Text::AddRelay))
                            .icon(Icon::Plus)
                            .style(if needs_relay {
                                ButtonStyle::Primary
                            } else {
                                ButtonStyle::Secondary
                            })
                            .compact()
                            .disabled(busy)
                            .on_press(cx.listener(|this, (), window, cx| {
                                this.open_setup_modal(
                                    SetupPage::Relay(RelayOperation::AddProfile),
                                    window,
                                    cx,
                                );
                                cx.notify();
                            })),
                    ),
            );
        if needs_relay {
            card = card.child(
                theme
                    .caption(locale.text(Text::NoEnabledRelays))
                    .text_color(Tone::Peach.text(theme)),
            );
        }
        if prefs.at_capacity {
            card = card.child(theme.caption(locale.text(Text::RelaySlotsFull)));
        }
        if let Some(progress) = self.inline_setup_banner(theme, cx) {
            card = card.child(progress);
        }
        if prefs.relays.is_empty() {
            card = card.child(
                row()
                    .px(px(14.))
                    .py(px(14.))
                    .rounded(px(12.))
                    .bg(theme.inset())
                    .child(theme.caption(locale.text(Text::NoCustomRelays))),
            );
        } else {
            // Keep rows inside a zero-gap container so divider lines sit symmetrically
            // between items instead of adding card gap above each border.
            let last = prefs.relays.len().saturating_sub(1);
            card = card.child(column().children(prefs.relays.iter().enumerate().map(
                |(index, relay)| {
                    self.network_relay_row(
                        theme,
                        relay,
                        index,
                        index == last,
                        busy,
                        prefs.at_capacity,
                        cx,
                    )
                },
            )));
        }
        if let Some(feedback) = self.network_inline_feedback(theme, cx) {
            card = card.child(feedback);
        }
        card
    }

    fn inline_setup_banner(&self, theme: Theme, cx: &Context<Self>) -> Option<Div> {
        let Runtime::Native(session) = &self.runtime else {
            return None;
        };
        let setup = &session.model(cx).setup;
        let setup_modal_open = self
            .overlays
            .last()
            .is_some_and(|frame| matches!(frame.content, Overlay::SharingSetup(..)));
        if setup_modal_open || super::settings_visible_setup(setup).is_none() {
            return None;
        }
        Some(
            column()
                .p(px(14.))
                .rounded(px(12.))
                .bg(theme.inset())
                .gap(px(10.))
                .child(self.setup_progress(theme, setup, cx)),
        )
    }

    fn network_inline_feedback(&self, theme: Theme, cx: &Context<Self>) -> Option<Div> {
        let setup_modal_open = self
            .overlays
            .last()
            .is_some_and(|frame| matches!(frame.content, Overlay::SharingSetup(..)));
        if setup_modal_open {
            return None;
        }
        let Runtime::Native(session) = &self.runtime else {
            return None;
        };
        let locale = language(cx);
        let feedback = self.network_ui.inline_feedback(session.model(cx))?;
        let (message, tone) = match feedback {
            Ok(label) => (locale.text(label).to_owned(), Tone::Mint),
            Err(error) => (error, Tone::Rose),
        };
        Some(theme.caption(message).text_color(tone.text(theme)))
    }

    #[allow(clippy::too_many_arguments)]
    fn network_relay_row(
        &self,
        theme: Theme,
        relay: &SavedRelay,
        index: usize,
        is_last: bool,
        busy: bool,
        at_capacity: bool,
        cx: &Context<Self>,
    ) -> Div {
        let can_toggle = !busy && (relay.enabled || !at_capacity);
        let expanded = self.network_ui.expanded_relay.as_deref() == Some(relay.url.as_str());
        let toggle_edit = if relay.enabled {
            PreferenceEdit::Disable(relay.url.clone())
        } else {
            PreferenceEdit::Enable(relay.url.clone())
        };
        let toggle = theme
            .toggle(("relay-switch", index), relay.enabled, can_toggle)
            .aria_label(relay.url.clone())
            .when(can_toggle, |control| {
                control.on_click(cx.listener(move |this, _, _, cx| {
                    cx.stop_propagation();
                    this.apply_preference(toggle_edit.clone(), cx);
                }))
            });
        let toggle_url = relay.url.clone();
        let trailing = row()
            .gap(px(10.))
            .child(relay_actions_toggle(
                index,
                expanded,
                busy,
                move |this: &mut Self, cx| {
                    this.network_ui.expanded_relay =
                        if this.network_ui.expanded_relay.as_deref() == Some(toggle_url.as_str()) {
                            None
                        } else {
                            Some(toggle_url.clone())
                        };
                    cx.notify();
                },
                cx,
            ))
            .child(toggle);
        column()
            .gap(px(10.))
            .pt(px(12.))
            .when(!is_last, |el| el.pb(px(12.)))
            .border_t_1()
            .border_color(theme.border())
            .child(
                row()
                    .gap(px(14.))
                    .child(theme.dot(if relay.enabled {
                        Tone::Mint
                    } else {
                        Tone::Neutral
                    }))
                    .child(
                        theme
                            .label(relay.url.clone(), 15.)
                            .font_family(FONT)
                            .flex_1()
                            .min_w_0()
                            .line_clamp(2),
                    )
                    .child(trailing),
            )
            .when(expanded, |col| {
                let reauth_url = relay.url.clone();
                let remove_url = relay.url.clone();
                col.child(relay_actions_drawer(
                    theme,
                    relay,
                    index,
                    busy,
                    self.runtime.is_preview(),
                    move |this: &mut Self, window, cx| {
                        this.confirm_reauthorize_relay(reauth_url.clone(), window, cx);
                    },
                    move |this: &mut Self, window, cx| {
                        this.confirm_remove_relay(remove_url.clone(), window, cx);
                    },
                    cx,
                ))
            })
    }

    fn receiver_site_card(
        theme: Theme,
        prefs: &RelayPreferences,
        busy: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let offered: BTreeSet<&str> = prefs
            .relays
            .iter()
            .filter_map(|relay| relay.offered_frontend.as_deref())
            .filter(|url| *url != prefs.frontend)
            .collect();
        let mut header_actions = row().gap(px(8.)).flex_wrap();
        if prefs.custom_frontend {
            header_actions = header_actions.child(
                Self::preference_button(
                    "default-website",
                    locale.text(Text::DefaultReceiver),
                    Icon::Refresh,
                    PreferenceEdit::Frontend(String::new()),
                    cx,
                )
                .style(ButtonStyle::Quiet)
                .disabled(busy),
            );
        }
        header_actions = header_actions.child(
            Button::new("edit-receiver-site", locale.text(Text::ChangeWebsite))
                .icon(Icon::Pencil)
                .style(ButtonStyle::Secondary)
                .compact()
                .disabled(busy)
                .on_press(cx.listener(|this, (), window, cx| {
                    this.open_setup_modal(SetupPage::ReceiverSite, window, cx);
                    cx.notify();
                })),
        );
        // Elevate the default chip against the inset strip so it stays legible in both themes.
        let status_chip = if prefs.custom_frontend {
            theme.badge(locale.text(Text::CustomBadge), Tone::Lavender)
        } else {
            theme
                .badge(locale.text(Text::DefaultBadge), Tone::Neutral)
                .bg(theme.surface())
                .border_1()
                .border_color(theme.border())
        };
        let card = theme
            .card()
            .px(px(CARD_PADDING_X))
            .py(px(CARD_PADDING_Y))
            .gap(px(14.))
            .child(
                row()
                    .justify_between()
                    .flex_wrap()
                    .gap(px(12.))
                    .child(
                        row()
                            .gap(px(12.))
                            .child(theme.icon_tile(Icon::Link, Tone::Lavender, CARD_ICON_SIZE))
                            .child(theme.heading(locale.text(Text::ReceiverSite), 18.)),
                    )
                    .child(header_actions),
            )
            .child(
                row()
                    .justify_between()
                    .gap(px(12.))
                    .px(px(14.))
                    .py(px(10.))
                    .rounded(px(12.))
                    .bg(theme.inset())
                    .border_1()
                    .border_color(theme.border().opacity(0.6))
                    .child(
                        theme
                            .label(prefs.frontend.clone(), 14.)
                            .font_family(FONT)
                            .flex_1()
                            .min_w_0()
                            .truncate(),
                    )
                    .child(status_chip),
            );
        if offered.is_empty() {
            card
        } else {
            card.child(Self::offered_receivers_section(theme, offered, busy, cx))
        }
    }

    fn offered_receivers_section(
        theme: Theme,
        offered: BTreeSet<&str>,
        busy: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        column()
            .gap(px(8.))
            .pt(px(12.))
            .border_t_1()
            .border_color(theme.border())
            .child(
                theme
                    .label(locale.text(Text::OfferedReceivers), 13.)
                    .font_weight(FontWeight::MEDIUM)
                    .text_color(theme.muted_text()),
            )
            .children(offered.into_iter().enumerate().map(|(index, url)| {
                row()
                    .gap(px(12.))
                    .child(
                        theme
                            .label(url.to_owned(), 14.)
                            .font_family(FONT)
                            .flex_1()
                            .min_w_0()
                            .line_clamp(2),
                    )
                    .child(
                        Self::preference_button(
                            ("offered-website", index),
                            locale.text(Text::UseReceiver),
                            Icon::Link,
                            PreferenceEdit::Frontend(url.to_owned()),
                            cx,
                        )
                        .disabled(busy),
                    )
            }))
    }
}
