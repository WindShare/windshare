use super::{Runtime, SetupPage};
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::Action,
};
use gpui::{Div, prelude::*, px};
use windshare_host::{PreferenceEdit, RelayPreferences};
use windshare_ui::{ButtonStyle, Icon, Theme, Tone, column, row};

impl Desktop {
    pub fn sharing_setup(&self, theme: Theme, id: Option<usize>, cx: &Context<Self>) -> Div {
        let Runtime::Native(session) = &self.runtime else {
            return column();
        };
        let locale = language(cx);
        let setup = &session.model.setup;
        let operation = id.and_then(|id| self.native_operation(id)).unwrap_or("");
        if let Some(progress) = self.setup_return(theme, operation, cx) {
            return progress;
        }
        let prefs = &session.model.preferences;
        let form = &self.setup_form;
        let locked = form.pending.is_some() || self.runtime.selection_disabled();
        let busy = locked || setup.unfinished();
        let share = id.and_then(|id| self.shares.iter().find(|share| share.id == id));
        let mut panel = column().gap(px(18.)).when_some(share, |panel, share| {
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
        let connecting = setup.context().is_some_and(|ctx| !ctx.relay_url.is_empty());
        if setup.busy() && connecting || setup.requires_cancellation() {
            return panel.child(Self::setup_progress(theme, setup, cx));
        }
        match form.page {
            SetupPage::Selection => {
                panel = panel.child(Self::relay_preferences(theme, prefs, busy, cx));
                if let Some(share) = share.filter(|share| !share.finished() && !share.live()) {
                    panel = panel.child(
                        Self::button(
                            "use-saved-sharing-settings",
                            locale.text(Text::UseSavedSettings),
                            Icon::Refresh,
                            ButtonStyle::Secondary,
                            Action::ResumeShare(share.id),
                            cx,
                        )
                        .disabled(busy),
                    );
                }
            }
            SetupPage::Relay(_) => {
                let blocked = locked || !setup.can_configure(operation);
                panel = panel.child(self.connect_relay_fields(theme, id, blocked, busy, cx));
            }
            SetupPage::ReceiverSite => {
                panel = panel.child(self.receiver_website(theme, prefs, id, busy, cx));
            }
        }
        if setup.unfinished() && !setup.busy() {
            panel = panel.child(Self::setup_progress(theme, setup, cx));
        } else if let Some(feedback) = &form.feedback {
            let (message, tone) = match feedback {
                Ok(label) => (locale.text(*label).to_owned(), Tone::Mint),
                Err(error) => (error.clone(), Tone::Rose),
            };
            panel = panel.child(theme.caption(message).text_color(tone.text(theme)));
        } else if let Some(error) = &session.model.command_error {
            panel = panel.child(
                theme
                    .caption(error.clone())
                    .text_color(Tone::Rose.text(theme)),
            );
        }
        panel
    }

    fn connect_relay_fields(
        &self,
        theme: Theme,
        id: Option<usize>,
        blocked: bool,
        busy: bool,
        cx: &Context<Self>,
    ) -> Div {
        let form = &self.setup_form;
        let locale = language(cx);
        column()
            .gap(px(16.))
            .child(
                column()
                    .gap(px(7.))
                    .child(theme.caption(locale.text(Text::RelayAddress)))
                    .child(form.relay.clone()),
            )
            .child(
                column()
                    .gap(px(7.))
                    .child(theme.caption(locale.text(Text::ServiceKey)))
                    .child(form.access_key.clone()),
            )
            .child(
                row()
                    .gap(px(8.))
                    .flex_wrap()
                    .child(
                        Self::button(
                            "connect-relay",
                            locale.text(form.page.title()),
                            Icon::Link,
                            ButtonStyle::Primary,
                            Action::SaveSharingSetup(id),
                            cx,
                        )
                        .disabled(blocked || form.relay.read(cx).value().trim().is_empty()),
                    )
                    .child(Self::back_to_relays(busy, cx)),
            )
    }

    fn receiver_website(
        &self,
        theme: Theme,
        prefs: &RelayPreferences,
        id: Option<usize>,
        busy: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let input = &self.setup_form.frontend;
        let url = input.read(cx).value().trim();
        let mut panel = column()
            .gap(px(16.))
            .child(theme.caption(locale.text(Text::ReceiverSiteDetail)))
            .child(
                column()
                    .gap(px(7.))
                    .child(theme.caption(locale.text(Text::ReceiverAddress)))
                    .child(input.clone()),
            )
            .child(
                row()
                    .flex_wrap()
                    .gap(px(8.))
                    .child(
                        Self::button(
                            "save-website",
                            locale.text(Text::SaveSettings),
                            Icon::Check,
                            ButtonStyle::Primary,
                            Action::SaveSharingSetup(id),
                            cx,
                        )
                        .disabled(busy || url.is_empty() || url == prefs.frontend),
                    )
                    .child(Self::back_to_relays(busy, cx)),
            );
        if prefs.custom_frontend {
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
        let offered: std::collections::BTreeSet<_> = prefs
            .relays
            .iter()
            .filter_map(|relay| relay.offered_frontend.as_deref())
            .filter(|url| *url != prefs.frontend)
            .collect();
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

    fn back_to_relays(busy: bool, cx: &Context<Self>) -> windshare_ui::Button {
        Self::button(
            "back-to-relays",
            language(cx).text(Text::BackToRelays),
            Icon::List,
            ButtonStyle::Quiet,
            Action::SetupPage(SetupPage::Selection),
            cx,
        )
        .disabled(busy)
    }
}
