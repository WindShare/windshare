use crate::{
    desktop::Desktop,
    live::Runtime,
    localization::{Text, language},
};
use gpui::{App, Context, Div, prelude::*, px};
use windshare_host::Setup;
use windshare_ui::{Button, ButtonStyle, Icon, Theme, Tone, column, row};

pub(super) fn setup_return_panel(
    theme: Theme,
    setup: &Setup,
    original_share_name: Option<&str>,
    return_button: Button,
    cx: &App,
) -> Div {
    let locale = language(cx);
    let progress = match setup {
        Setup::Running {
            verification_url, ..
        } if !verification_url.is_empty() => Text::AuthorizationRequired,
        Setup::Cancelling { .. } => Text::CancellingSetup,
        Setup::Failed { .. } | Setup::CancelFailed { .. } => Text::SetupNeedsAttention,
        _ => Text::ConnectingRelay,
    };
    column()
        .gap(px(12.))
        .child(theme.caption(locale.text(progress)))
        .when_some(original_share_name, |view, name| {
            view.child(theme.label(name.to_owned(), 16.))
        })
        .child(return_button)
}

pub(super) fn setup_progress_panel<V: 'static>(
    theme: Theme,
    setup: &Setup,
    approve_owner: impl FnOnce(&Context<V>) -> Button,
    approve_device: impl FnOnce(&Context<V>) -> Button,
    guest: impl FnOnce(&Context<V>) -> Button,
    cancel_button: impl FnOnce(Text, &Context<V>) -> Button,
    cx: &Context<V>,
) -> Div {
    let locale = language(cx);
    let mut panel = column().gap(px(12.));
    match setup {
        Setup::Failed { error, .. } | Setup::CancelFailed { error, .. } => {
            panel = panel.child(
                theme
                    .label(error.message.clone(), 15.)
                    .text_color(Tone::Rose.text(theme)),
            );
        }
        Setup::Cancelling { .. } => {
            panel = panel.child(theme.caption(locale.text(Text::CancellingSetup)));
        }
        Setup::Running {
            verification_url,
            user_code,
            service_login,
            guest_available,
            ..
        } if !verification_url.is_empty() => {
            if let Some(login) = service_login {
                let url = login.url.clone();
                panel = panel
                    .child(theme.label(login.host.clone(), 15.))
                    .child(theme.caption(locale.text(Text::ServiceCodeDetail)))
                    .child(theme.heading(user_code.clone(), 18.))
                    .child(
                        row()
                            .gap(px(8.))
                            .flex_wrap()
                            .child(
                                Button::new("service-login", locale.text(Text::ServiceLogin))
                                    .icon(Icon::External)
                                    .style(ButtonStyle::Primary)
                                    .on_press(move |(), _, cx| cx.open_url(&url)),
                            )
                            .child(approve_owner(cx)),
                    );
            } else {
                panel = panel
                    .child(theme.caption(locale.text(Text::RelayApprovalDetail)))
                    .child(
                        row()
                            .gap(px(12.))
                            .child(theme.heading(user_code.clone(), 18.))
                            .child(approve_device(cx)),
                    );
            }

            if *guest_available {
                panel = panel.child(guest(cx));
            }
        }
        Setup::Running { .. } => {
            panel = panel.child(theme.caption(locale.text(Text::ConnectingRelay)));
        }
        _ => return panel,
    }
    let cancel_label = if matches!(setup, Setup::CancelFailed { .. }) {
        Text::RetrySetupCancellation
    } else {
        Text::CancelSetup
    };
    panel.child(cancel_button(cancel_label, cx))
}

impl Desktop {
    fn setup_cancel_disabled(&self, cx: &App) -> bool {
        match &self.runtime {
            Runtime::Native(session) => {
                let store = session.host.read(cx);
                store.selection_disabled() || !store.model().can_cancel_current_setup()
            }
            Runtime::Preview(_) => true,
        }
    }

    fn setup_guest_disabled(&self, cx: &App) -> bool {
        match &self.runtime {
            Runtime::Native(session) => {
                let store = session.host.read(cx);
                store.selection_disabled() || !store.model().can_guest_current_setup()
            }
            Runtime::Preview(_) => true,
        }
    }

    pub fn community_paused_notice(&self, theme: Theme, cx: &Context<Self>) -> Option<Div> {
        if !self.community_notice.visible() {
            return None;
        }
        let locale = language(cx);
        Some(
            theme
                .card()
                .px(px(18.))
                .py(px(14.))
                .border_color(Tone::Peach.accent(theme).opacity(0.45))
                .child(
                    row()
                        .gap(px(12.))
                        .justify_between()
                        .child(
                            row()
                                .flex_1()
                                .min_w_0()
                                .gap(px(10.))
                                .child(theme.dot(Tone::Peach))
                                .child(
                                    theme
                                        .label(locale.text(Text::CommunityPaused), 14.)
                                        .flex_1()
                                        .min_w_0(),
                                ),
                        )
                        .child(
                            Button::new(
                                "dismiss-community-paused-notice",
                                locale.text(Text::GotIt),
                            )
                            .style(ButtonStyle::Quiet)
                            .compact()
                            .on_press(cx.listener(
                                |this, (), window, cx| {
                                    this.dispatch(
                                        crate::model::Action::DismissCommunityNotice,
                                        window,
                                        cx,
                                    );
                                },
                            )),
                        ),
                ),
        )
    }

    pub fn setup_cancellation_notice(&self, theme: Theme, error: &str, cx: &Context<Self>) -> Div {
        theme
            .card()
            .p(px(18.))
            .gap(px(12.))
            .child(
                theme
                    .label(error.to_owned(), 15.)
                    .text_color(Tone::Rose.text(theme)),
            )
            .child(
                Button::new(
                    "retry-setup-cancellation",
                    language(cx).text(Text::RetrySetupCancellation),
                )
                .icon(Icon::Refresh)
                .style(ButtonStyle::Secondary)
                .disabled(self.setup_cancel_disabled(cx))
                .on_press(cx.listener(|this, (), _, cx| {
                    this.cancel_sharing_setup(cx);
                    cx.notify();
                })),
            )
    }

    pub(crate) fn setup_return(
        &self,
        theme: Theme,
        operation: &str,
        cx: &Context<Self>,
    ) -> Option<Div> {
        let Runtime::Native(session) = &self.runtime else {
            return None;
        };
        let setup = &session.model(cx).setup;
        let context = setup.context()?;
        if !setup.unfinished() || operation.is_empty() || context.operation_id == operation {
            return None;
        }
        Some(self.setup_return_for_context(theme, setup, context, cx))
    }

    pub(super) fn setup_return_for_context(
        &self,
        theme: Theme,
        setup: &Setup,
        context: &windshare_host::SetupContext,
        cx: &Context<Self>,
    ) -> Div {
        let original = self.shares.iter().find(|share| {
            matches!(&share.source, crate::model::ShareSource::Native { operation_id } if operation_id == &context.operation_id)
        });
        let button = Button::new(
            "return-to-running-setup",
            language(cx).text(Text::ReturnToSetup),
        )
        .icon(Icon::Settings)
        .style(ButtonStyle::Secondary)
        .on_press(cx.listener(|this, (), window, cx| {
            this.return_to_sharing_setup(window, cx);
            cx.notify();
        }));
        setup_return_panel(
            theme,
            setup,
            original.map(|share| share.name.as_str()),
            button,
            cx,
        )
    }

    pub(super) fn setup_progress(&self, theme: Theme, setup: &Setup, cx: &Context<Self>) -> Div {
        let guest_disabled = self.setup_guest_disabled(cx);
        let cancel_disabled = self.setup_cancel_disabled(cx);
        setup_progress_panel(
            theme,
            setup,
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
                    .disabled(guest_disabled)
                    .on_press(cx.listener(|this, (), _, cx| {
                        this.guest_sharing_setup(cx);
                        cx.notify();
                    }))
            },
            |label, cx| {
                Button::new("cancel-sharing-setup", language(cx).text(label))
                    .icon(Icon::Close)
                    .style(ButtonStyle::Secondary)
                    .disabled(cancel_disabled)
                    .on_press(cx.listener(|this, (), _, cx| {
                        this.cancel_sharing_setup(cx);
                        cx.notify();
                    }))
            },
            cx,
        )
    }
}
