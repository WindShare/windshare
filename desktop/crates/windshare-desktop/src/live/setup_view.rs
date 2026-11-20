use super::{Runtime, SetupPage};
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::{Action, SharingSetupTarget},
};
use gpui::{Div, prelude::*, px};
use windshare_host::{PreferenceEdit, RelayPreferences};
use windshare_ui::{ButtonStyle, Icon, Theme, Tone, column, row};

impl Desktop {
    pub fn sharing_setup(
        &self,
        theme: Theme,
        target: SharingSetupTarget,
        cx: &Context<Self>,
    ) -> Div {
        let prefs = self.relay_preferences_view();
        let locale = language(cx);
        let idle = windshare_host::Setup::Idle;
        let id = target.share_id();
        let operation = id.and_then(|id| self.native_operation(id)).unwrap_or("");
        let (setup, command_error, waits_official) = match &self.runtime {
            Runtime::Native(session) => (
                &session.model.setup,
                session.model.command_error.as_ref(),
                session.model.waits_for_official_authorization(operation),
            ),
            Runtime::Preview(_) => (&idle, None, false),
        };
        if let Some(progress) = self.setup_return(theme, operation, cx) {
            return progress;
        }
        let form = &self.setup_form;
        let locked = form.pending.is_some() || self.runtime.selection_disabled();
        let busy = locked || setup.unfinished();
        let share = id.and_then(|id| self.shares.iter().find(|share| share.id == id));
        let switch_share = match target {
            SharingSetupTarget::SwitchRelay(id) => Some(id),
            _ => None,
        };
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
        let close_on_back = matches!(target, SharingSetupTarget::Defaults);
        panel = match form.page {
            SetupPage::Selection => panel
                .child(self.relay_preferences(
                    theme,
                    prefs,
                    switch_share,
                    busy || switch_share.is_some() && !waits_official,
                    cx,
                ))
                .when_some(
                    share.filter(|share| {
                        switch_share.is_none() && !share.finished() && !share.live()
                    }),
                    |panel, share| {
                        panel.child(
                            Self::button(
                                "use-saved-sharing-settings",
                                locale.text(Text::UseSavedSettings),
                                Icon::Refresh,
                                ButtonStyle::Secondary,
                                Action::UseSavedSettings(share.id),
                                cx,
                            )
                            .disabled(busy),
                        )
                    },
                ),
            SetupPage::Relay(_) => panel.child(self.connect_relay_fields(
                theme,
                id,
                locked || !setup.can_configure(operation),
                busy,
                close_on_back,
                cx,
            )),
            SetupPage::ReceiverSite => {
                panel.child(self.receiver_website(theme, prefs, id, busy, close_on_back, cx))
            }
        };
        if let Some(feedback) = self.sharing_setup_feedback(theme, setup, command_error, cx) {
            panel = panel.child(feedback);
        }
        panel
    }

    fn sharing_setup_feedback(
        &self,
        theme: Theme,
        setup: &windshare_host::Setup,
        command_error: Option<&String>,
        cx: &Context<Self>,
    ) -> Option<Div> {
        let locale = language(cx);
        if setup.unfinished() && !setup.busy() {
            Some(Self::setup_progress(theme, setup, cx))
        } else if let Some(feedback) = &self.setup_form.feedback {
            let (message, tone) = match feedback {
                Ok(label) => (locale.text(*label).to_owned(), Tone::Mint),
                Err(error) => (error.clone(), Tone::Rose),
            };
            Some(theme.caption(message).text_color(tone.text(theme)))
        } else {
            command_error.map(|error| {
                theme
                    .caption(error.clone())
                    .text_color(Tone::Rose.text(theme))
            })
        }
    }

    fn connect_relay_fields(
        &self,
        theme: Theme,
        id: Option<usize>,
        blocked: bool,
        busy: bool,
        close_on_back: bool,
        cx: &Context<Self>,
    ) -> Div {
        let form = &self.setup_form;
        let locale = language(cx);
        let input_ready = form.input_submittable("", cx);
        form.rendered_input_ready.set(Some(input_ready));
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
                        .disabled(blocked || !input_ready),
                    )
                    .child(Self::back_to_relays(busy, close_on_back, cx)),
            )
    }

    fn receiver_website(
        &self,
        theme: Theme,
        prefs: &RelayPreferences,
        id: Option<usize>,
        busy: bool,
        close_on_back: bool,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let input = &self.setup_form.frontend;
        let input_ready = self.setup_form.input_submittable(&prefs.frontend, cx);
        self.setup_form.rendered_input_ready.set(Some(input_ready));
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
                        .disabled(busy || !input_ready),
                    )
                    .child(Self::back_to_relays(busy, close_on_back, cx)),
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
        let offered: std::collections::BTreeSet<_> = if close_on_back {
            std::collections::BTreeSet::new()
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

    fn back_to_relays(busy: bool, close_on_back: bool, cx: &Context<Self>) -> windshare_ui::Button {
        let (label, icon, action) = if close_on_back {
            (Text::Cancel, Icon::Close, Action::Close)
        } else {
            (
                Text::BackToRelays,
                Icon::ArrowLeft,
                Action::SetupPage(SetupPage::Selection),
            )
        };
        Self::button(
            "back-to-relays",
            language(cx).text(label),
            icon,
            ButtonStyle::Quiet,
            action,
            cx,
        )
        .disabled(busy)
    }
}
