use super::{
    SetupDialog, SetupDialogEvent, SetupPage,
    relay_preferences::{
        community_toggle, relay_actions_drawer, relay_actions_toggle, relay_mode_option,
    },
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
        if let Some(other) = self.other_running_setup(cx) {
            return setup_return_panel(
                theme,
                self.setup(cx),
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
        let locked = self.pending(cx) || self.selection_disabled(cx) || self.share_finished(cx);
        let busy = self.busy(cx);
        let switch_share = match self.target {
            SharingSetupTarget::SwitchRelay(id) => Some(id),
            _ => None,
        };
        let share = self.share_summary(cx);
        let mut panel = column()
            .gap(px(18.))
            .when_some(share.as_ref(), |panel, share| {
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
        if self.shows_setup_progress(cx) {
            return panel.child(self.progress_view(theme, cx));
        }
        let close_on_back = matches!(self.target, SharingSetupTarget::Defaults);
        panel = match self.page {
            SetupPage::Selection => {
                self.selection_body(panel, theme, share.as_ref(), switch_share, busy, cx)
            }
            SetupPage::Relay(_) => panel.child(self.connect_relay_fields(
                theme,
                locked || !self.setup(cx).can_configure(self.operation_id()),
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
        let cancel_disabled = self.cancel_disabled(cx);
        setup_progress_panel(
            theme,
            self.setup(cx),
            |cx| {
                Button::new("approve-owner", language(cx).text(Text::ApproveAsOwner))
                    .icon(Icon::Shield)
                    .style(ButtonStyle::Quiet)
                    .on_press(cx.listener(|this, (), _, cx| this.approve_relay(cx)))
            },
            |cx| {
                Button::new("approve-device", language(cx).text(Text::ApproveDevice))
                    .icon(Icon::External)
                    .style(ButtonStyle::Primary)
                    .on_press(cx.listener(|this, (), _, cx| this.approve_relay(cx)))
            },
            |cx| {
                Button::new("connect-as-guest", language(cx).text(Text::ConnectAsGuest))
                    .style(ButtonStyle::Quiet)
                    .on_press(cx.listener(|this, (), _, cx| {
                        if let super::dialog::SetupSource::Native { host, .. } = &this.source
                            && let Some(context) = this.setup(cx).context()
                        {
                            host.read(cx).controller().guest_setup(&context.id);
                        }
                    }))
            },
            |label, cx| {
                Button::new("cancel-sharing-setup", language(cx).text(label))
                    .icon(Icon::Close)
                    .style(ButtonStyle::Secondary)
                    .disabled(cancel_disabled)
                    .on_press(cx.listener(|this, (), _, cx| this.cancel_setup(cx)))
            },
            cx,
        )
    }

    fn selection_body(
        &self,
        panel: Div,
        theme: Theme,
        share: Option<&super::dialog_host::ShareDisplay>,
        switch_share: Option<usize>,
        busy: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let prefs_busy = busy || switch_share.is_some() && !self.waits_official(cx);
        panel
            .child(self.relay_preferences_panel(theme, switch_share, prefs_busy, cx))
            .when(
                share.is_some_and(|share| switch_share.is_none() && !share.finished && !share.live),
                |panel| {
                    panel.child(
                        Button::new(
                            "use-saved-sharing-settings",
                            locale.text(Text::UseSavedSettings),
                        )
                        .icon(Icon::Refresh)
                        .style(ButtonStyle::Secondary)
                        .disabled(busy)
                        .on_press(cx.listener(|this, (), _, cx| this.use_saved_settings(cx))),
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
        let prefs = self.prefs(cx);
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
                        .on_press(cx.listener(|this, (), _, cx| {
                            this.set_page(SetupPage::Relay(RelayOperation::AddProfile), cx);
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
                        |this, cx| {
                            this.apply_preference(PreferenceEdit::Mode(RelayMode::Automatic), cx);
                        },
                        cx,
                    ))
                    .child(relay_mode_option(
                        theme,
                        prefs,
                        RelayMode::Selected,
                        busy,
                        false,
                        |this, cx| {
                            this.apply_preference(PreferenceEdit::Mode(RelayMode::Selected), cx);
                        },
                        cx,
                    )),
            )
            .when(prefs.mode == RelayMode::Automatic, |panel| {
                panel.child(community_toggle(
                    theme,
                    prefs,
                    busy,
                    Self::apply_preference,
                    cx,
                ))
            })
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
                            .on_press(cx.listener(|this, (), _, cx| {
                                this.set_page(SetupPage::ReceiverSite, cx);
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
        let at_capacity = self.prefs(cx).at_capacity;
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
            |_| {
                let relay_url = relay.url.clone();
                Button::new(("use-relay", index), locale.text(Text::UseRelayForShare))
                    .icon(Icon::Arrow)
                    .compact()
                    .style(ButtonStyle::Secondary)
                    .disabled(busy)
                    .on_press(cx.listener(move |this, (), _, cx| {
                        this.switch_share_relay(&relay_url, cx);
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
        let expanded =
            switch_share.is_none() && self.expanded_relay.as_deref() == Some(relay.url.as_str());
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
                    move |this, cx| this.toggle_relay_actions(toggle_url.clone(), cx),
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
                    self.is_preview(),
                    move |this, window, cx| {
                        this.confirm_reauthorize_relay(reauth_url.clone(), window, cx);
                    },
                    move |this, window, cx| {
                        this.confirm_remove_relay(remove_url.clone(), window, cx);
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
            .on_press(cx.listener(move |this, (), _, cx| {
                this.apply_preference(edit.clone(), cx);
            }))
    }

    fn feedback_view(&self, theme: Theme, cx: &Context<Self>) -> Option<Div> {
        let locale = language(cx);
        let setup = self.setup(cx);
        if setup.unfinished() && !setup.busy() {
            Some(self.progress_view(theme, cx))
        } else if let Some(feedback) = &self.feedback {
            let (message, tone) = match feedback {
                Ok(label) => (locale.text(*label).to_owned(), Tone::Mint),
                Err(error) => (error.clone(), Tone::Rose),
            };
            Some(theme.caption(message).text_color(tone.text(theme)))
        } else {
            self.command_error(cx).map(|error| {
                theme
                    .caption(error.to_owned())
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
        let prefs = self.prefs(cx);
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
            .on_press(cx.listener(move |this, (), _, cx| {
                if close_on_back {
                    cx.emit(SetupDialogEvent::Close);
                } else {
                    this.set_page(SetupPage::Selection, cx);
                }
            }))
    }
}
