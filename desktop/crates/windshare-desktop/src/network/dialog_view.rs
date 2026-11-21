use super::{
    SetupDialog, SetupDialogEvent, SetupPage,
    relay_preferences::{relay_actions_drawer, relay_actions_toggle, relay_mode_option},
    setup_feedback::{setup_progress_panel, setup_return_panel},
};
use crate::{
    localization::{Text, language},
    model::SharingSetupTarget,
};
use gpui::{Context, Div, ElementId, Window, prelude::*, px};
use std::collections::BTreeSet;
use windshare_host::{PreferenceEdit, RelayMode, RelayOperation, SavedRelay};
use windshare_ui::{Button, ButtonStyle, Icon, Theme, Tone, column, row};

impl Render for SetupDialog {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = Theme::current(window, cx);
        let locale = language(cx);
        let snapshot = &self.snapshot;
        if let Some(other) = &snapshot.other_running {
            return setup_return_panel(
                theme,
                &snapshot.setup,
                other.share_name.as_deref(),
                Button::new("return-to-running-setup", locale.text(Text::ReturnToSetup))
                    .icon(Icon::Settings)
                    .style(ButtonStyle::Secondary)
                    .on_press(
                        cx.listener(|_, (), _, cx| cx.emit(SetupDialogEvent::ReturnToRunningSetup)),
                    ),
                cx,
            );
        }
        let locked = snapshot.mode.pending() || snapshot.mode.selection_disabled();
        let busy = locked || snapshot.setup.unfinished();
        let switch_share = match snapshot.target {
            SharingSetupTarget::SwitchRelay(id) => Some(id),
            _ => None,
        };
        let mut panel = column()
            .gap(px(18.))
            .when_some(snapshot.share.as_ref(), |panel, share| {
                panel.child(
                    row()
                        .gap(px(10.))
                        .child(theme.icon_tile(share.symbol, Tone::Mint, 32.))
                        .child(
                            theme
                                .label(share.name.clone(), 15.)
                                .flex_1()
                                .min_w_0()
                                .line_clamp(2),
                        ),
                )
            });
        if self.shows_setup_progress() {
            return panel.child(self.progress_view(theme, cx));
        }
        let close_on_back = matches!(snapshot.target, SharingSetupTarget::Defaults);
        panel = match self.page {
            SetupPage::Selection => self.selection_body(panel, theme, switch_share, busy, cx),
            SetupPage::Relay(_) => panel.child(self.connect_relay_fields(
                theme,
                locked || !snapshot.setup.can_configure(self.operation_id()),
                busy,
                close_on_back,
                cx,
            )),
            SetupPage::ReceiverSite => {
                panel.child(self.receiver_website(theme, busy, close_on_back, cx))
            }
        };
        if let Some(feedback) = self.feedback_view(theme, cx) {
            panel = panel.child(feedback);
        }
        panel
    }
}

impl SetupDialog {
    fn progress_view(&self, theme: Theme, cx: &Context<Self>) -> Div {
        setup_progress_panel(
            theme,
            &self.snapshot.setup,
            |cx| {
                Button::new("approve-owner", language(cx).text(Text::ApproveAsOwner))
                    .icon(Icon::Shield)
                    .style(ButtonStyle::Quiet)
                    .on_press(cx.listener(|_, (), _, cx| cx.emit(SetupDialogEvent::ApproveRelay)))
            },
            |cx| {
                Button::new("approve-device", language(cx).text(Text::ApproveDevice))
                    .icon(Icon::External)
                    .style(ButtonStyle::Primary)
                    .on_press(cx.listener(|_, (), _, cx| cx.emit(SetupDialogEvent::ApproveRelay)))
            },
            |label, disabled, cx| {
                Button::new("cancel-sharing-setup", language(cx).text(label))
                    .icon(Icon::Close)
                    .style(ButtonStyle::Secondary)
                    .disabled(disabled)
                    .on_press(cx.listener(|_, (), _, cx| cx.emit(SetupDialogEvent::CancelSetup)))
            },
            cx,
        )
    }

    fn selection_body(
        &self,
        panel: Div,
        theme: Theme,
        switch_share: Option<usize>,
        busy: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let prefs_busy = busy || switch_share.is_some() && !self.snapshot.mode.waits_official();
        panel
            .child(self.relay_preferences_panel(theme, switch_share, prefs_busy, cx))
            .when_some(
                self.snapshot
                    .share
                    .as_ref()
                    .filter(|share| switch_share.is_none() && !share.finished && !share.live),
                |panel, share| {
                    let id = share.id;
                    panel.child(
                        Button::new(
                            "use-saved-sharing-settings",
                            locale.text(Text::UseSavedSettings),
                        )
                        .icon(Icon::Refresh)
                        .style(ButtonStyle::Secondary)
                        .disabled(busy)
                        .on_press(cx.listener(move |_, (), _, cx| {
                            cx.emit(SetupDialogEvent::UseSavedSettings(id));
                        })),
                    )
                },
            )
    }

    fn relay_preferences_panel(
        &self,
        theme: Theme,
        switch_share: Option<usize>,
        busy: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let prefs = &self.snapshot.prefs;
        let enabled = prefs.relays.iter().any(|relay| relay.enabled);
        let mut list = column().gap(px(12.)).child(
            row()
                .justify_between()
                .gap(px(12.))
                .child(theme.heading(locale.text(Text::CustomRelays), 17.))
                .child(
                    Button::new("add-relay", locale.text(Text::AddRelay))
                        .icon(Icon::Plus)
                        .style(ButtonStyle::Secondary)
                        .compact()
                        .disabled(busy)
                        .on_press(cx.listener(|this, (), window, cx| {
                            this.set_page(SetupPage::Relay(RelayOperation::AddProfile), window, cx);
                        })),
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
            self.saved_relay_row(theme, relay, index, switch_share, busy, cx)
        }));
        if switch_share.is_some() {
            return list;
        }
        column()
            .gap(px(20.))
            .child(
                column()
                    .id("relay-modes")
                    .role(gpui::Role::RadioGroup)
                    .aria_label(locale.text(Text::RelayChoice))
                    .gap(px(8.))
                    .child(relay_mode_option(
                        theme,
                        prefs,
                        RelayMode::Automatic,
                        busy,
                        false,
                        |_, cx| {
                            cx.emit(SetupDialogEvent::EditPreference(PreferenceEdit::Mode(
                                RelayMode::Automatic,
                            )));
                        },
                        cx,
                    ))
                    .child(relay_mode_option(
                        theme,
                        prefs,
                        RelayMode::Selected,
                        busy,
                        false,
                        |_, cx| {
                            cx.emit(SetupDialogEvent::EditPreference(PreferenceEdit::Mode(
                                RelayMode::Selected,
                            )));
                        },
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
                        Button::new("edit-receiver-site", locale.text(Text::ChangeWebsite))
                            .icon(Icon::Link)
                            .style(ButtonStyle::Quiet)
                            .compact()
                            .disabled(busy)
                            .on_press(cx.listener(|this, (), window, cx| {
                                this.set_page(SetupPage::ReceiverSite, window, cx);
                            })),
                    ),
            )
    }

    fn saved_relay_choice(
        &self,
        relay: &SavedRelay,
        index: usize,
        switch_share: Option<usize>,
        busy: bool,
        cx: &Context<Self>,
    ) -> Button {
        let locale = language(cx);
        let at_capacity = self.snapshot.prefs.at_capacity;
        switch_share.map_or_else(
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
            |share_id| {
                let relay_url = relay.url.clone();
                Button::new(("use-relay", index), locale.text(Text::UseRelayForShare))
                    .icon(Icon::Arrow)
                    .compact()
                    .style(ButtonStyle::Secondary)
                    .disabled(busy)
                    .on_press(cx.listener(move |_, (), _, cx| {
                        cx.emit(SetupDialogEvent::SwitchShareRelay {
                            share_id,
                            relay_url: relay_url.clone(),
                        });
                    }))
            },
        )
    }

    fn saved_relay_row(
        &self,
        theme: Theme,
        relay: &SavedRelay,
        index: usize,
        switch_share: Option<usize>,
        busy: bool,
        cx: &Context<Self>,
    ) -> Div {
        let expanded = switch_share.is_none()
            && self.snapshot.expanded_relay.as_deref() == Some(relay.url.as_str());
        let choice = self.saved_relay_choice(relay, index, switch_share, busy, cx);
        let toggle_url = relay.url.clone();
        let actions = row()
            .gap(px(6.))
            .flex_wrap()
            .justify_end()
            .when(switch_share.is_none(), |actions| {
                actions.child(relay_actions_toggle(
                    index,
                    expanded,
                    busy,
                    move |_, cx| {
                        cx.emit(SetupDialogEvent::ToggleRelayActions(toggle_url.clone()));
                    },
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
                let reauth_url = relay.url.clone();
                let remove_url = relay.url.clone();
                col.child(relay_actions_drawer(
                    theme,
                    relay,
                    index,
                    busy,
                    self.snapshot.mode.is_preview(),
                    move |_, _, cx| {
                        cx.emit(SetupDialogEvent::ConfirmReauthorizeRelay(
                            reauth_url.clone(),
                        ));
                    },
                    move |_, _, cx| {
                        cx.emit(SetupDialogEvent::ConfirmRemoveRelay(remove_url.clone()));
                    },
                    cx,
                ))
            })
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
            .on_press(cx.listener(move |_, (), _, cx| {
                cx.emit(SetupDialogEvent::EditPreference(edit.clone()));
            }))
    }

    fn feedback_view(&self, theme: Theme, cx: &Context<Self>) -> Option<Div> {
        let locale = language(cx);
        let setup = &self.snapshot.setup;
        if setup.unfinished() && !setup.busy() {
            Some(self.progress_view(theme, cx))
        } else if let Some(feedback) = &self.snapshot.feedback {
            let (message, tone) = match feedback {
                Ok(label) => (locale.text(*label).to_owned(), Tone::Mint),
                Err(error) => (error.clone(), Tone::Rose),
            };
            Some(theme.caption(message).text_color(tone.text(theme)))
        } else {
            self.snapshot.command_error.as_ref().map(|error| {
                theme
                    .caption(error.clone())
                    .text_color(Tone::Rose.text(theme))
            })
        }
    }

    fn connect_relay_fields(
        &self,
        theme: Theme,
        blocked: bool,
        busy: bool,
        close_on_back: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let input_ready = self.input_submittable(cx);
        column()
            .gap(px(16.))
            .child(
                column()
                    .gap(px(7.))
                    .child(theme.caption(locale.text(Text::RelayAddress)))
                    .child(self.relay.clone()),
            )
            .child(
                column()
                    .gap(px(7.))
                    .child(theme.caption(locale.text(Text::ServiceKey)))
                    .child(self.access_key.clone()),
            )
            .child(
                row()
                    .gap(px(8.))
                    .flex_wrap()
                    .child(
                        Button::new("connect-relay", locale.text(self.page.title()))
                            .icon(Icon::Link)
                            .style(ButtonStyle::Primary)
                            .disabled(blocked || !input_ready)
                            .on_press(cx.listener(|this, (), _, cx| this.submit(cx))),
                    )
                    .child(Self::back_button(busy, close_on_back, cx)),
            )
    }

    fn receiver_website(
        &self,
        theme: Theme,
        busy: bool,
        close_on_back: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let prefs = &self.snapshot.prefs;
        let input_ready = self.input_submittable(cx);
        let mut panel = column()
            .gap(px(16.))
            .child(theme.caption(locale.text(Text::ReceiverSiteDetail)))
            .child(
                column()
                    .gap(px(7.))
                    .child(theme.caption(locale.text(Text::ReceiverAddress)))
                    .child(self.frontend.clone()),
            )
            .child(
                row()
                    .flex_wrap()
                    .gap(px(8.))
                    .child(
                        Button::new("save-website", locale.text(Text::SaveSettings))
                            .icon(Icon::Check)
                            .style(ButtonStyle::Primary)
                            .disabled(busy || !input_ready)
                            .on_press(cx.listener(|this, (), _, cx| this.submit(cx))),
                    )
                    .child(Self::back_button(busy, close_on_back, cx)),
            );
        if !close_on_back && prefs.custom_frontend {
            panel = panel.child(
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
        let offered: BTreeSet<_> = if close_on_back {
            BTreeSet::new()
        } else {
            prefs
                .relays
                .iter()
                .filter_map(|relay| relay.offered_frontend.as_deref())
                .filter(|url| *url != prefs.frontend)
                .collect()
        };
        if !offered.is_empty() {
            panel = panel
                .child(theme.separator())
                .child(theme.heading(locale.text(Text::OfferedReceivers), 16.));
        }
        for (i, url) in offered.into_iter().enumerate() {
            panel = panel.child(
                row()
                    .gap(px(12.))
                    .child(
                        theme
                            .caption(url.to_owned())
                            .flex_1()
                            .min_w_0()
                            .line_clamp(2),
                    )
                    .child(
                        Self::preference_button(
                            ("offered-website", i),
                            locale.text(Text::UseReceiver),
                            Icon::Link,
                            PreferenceEdit::Frontend(url.to_owned()),
                            cx,
                        )
                        .disabled(busy),
                    ),
            );
        }
        panel
    }

    fn back_button(busy: bool, close_on_back: bool, cx: &Context<Self>) -> Button {
        let (label, icon) = if close_on_back {
            (Text::Cancel, Icon::Close)
        } else {
            (Text::BackToRelays, Icon::ArrowLeft)
        };
        Button::new("back-to-relays", language(cx).text(label))
            .icon(icon)
            .style(ButtonStyle::Quiet)
            .disabled(busy)
            .on_press(cx.listener(move |this, (), window, cx| {
                if close_on_back {
                    cx.emit(SetupDialogEvent::Close);
                } else {
                    this.set_page(SetupPage::Selection, window, cx);
                }
            }))
    }
}
