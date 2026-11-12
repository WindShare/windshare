use super::Runtime;
use crate::{
    desktop::Desktop,
    flow_state::{ShareFlow, ShareReadiness, ShareRun},
    localization::{Text, language},
    model::{Action, Page, Share, ShareSource},
};
use gpui::{Div, Entity, Subscription, Window, prelude::*, px};
use std::time::Instant;
use windshare_host::Setup;
use windshare_ui::{ButtonStyle, Icon, Input, InputEvent, Theme, Tone, column, row};

const SETUP_WIDTH: f32 = 560.;

pub struct SetupForm {
    relay: Entity<Input>,
    frontend: Entity<Input>,
    _subscriptions: Vec<Subscription>,
}

impl SetupForm {
    pub fn new(cx: &mut Context<Desktop>) -> Self {
        let locale = language(cx);
        let relay = cx.new(|cx| {
            Input::new(locale.text(Text::RelayAddress), cx).placeholder("https://relay.example")
        });
        let frontend = cx.new(|cx| {
            Input::new(locale.text(Text::FrontendAddress), cx).placeholder("https://share.example")
        });
        let subscriptions = [&relay, &frontend]
            .into_iter()
            .map(|input| {
                cx.subscribe(input, |this, _, event, cx| {
                    if matches!(event, InputEvent::Submit)
                        && let ShareFlow::Running(run) = this.share_flow
                    {
                        this.save_sharing_setup(run.selection_id, cx);
                    }
                    cx.notify();
                })
            })
            .collect();
        Self {
            relay,
            frontend,
            _subscriptions: subscriptions,
        }
    }

    pub fn localize(&self, cx: &mut Context<Desktop>) {
        let locale = language(cx);
        self.relay.update(cx, |input, cx| {
            input.set_labels(locale.text(Text::RelayAddress), "https://relay.example", cx);
        });
        self.frontend.update(cx, |input, cx| {
            input.set_labels(
                locale.text(Text::FrontendAddress),
                "https://share.example",
                cx,
            );
        });
    }
}

impl Desktop {
    fn native_operation(&self, id: usize) -> Option<&str> {
        self.shares
            .iter()
            .find(|share| share.id == id)
            .and_then(|share| {
                if let ShareSource::Native { operation_id } = &share.source {
                    Some(operation_id.as_str())
                } else {
                    None
                }
            })
    }

    pub fn open_sharing_setup(&mut self, id: usize, window: &mut Window, cx: &mut Context<Self>) {
        self.close_overlay(window, cx);
        self.share_flow = ShareFlow::Running(ShareRun {
            operation: id,
            selection_id: id,
            started_at: Instant::now(),
            readiness: ShareReadiness::Pending,
        });
        self.navigate(Page::Share, window, cx);
    }

    pub fn save_sharing_setup(&self, id: usize, cx: &Context<Self>) {
        if let Runtime::Native(session) = &self.runtime
            && !matches!(session.model.setup, Setup::Running { .. })
            && let Some(operation_id) = self.native_operation(id)
        {
            session.controller.configure(
                operation_id,
                self.setup_form.relay.read(cx).value().trim().into(),
                self.setup_form.frontend.read(cx).value().trim().into(),
            );
        }
    }

    pub fn resume_native_share(&self, id: usize) {
        if let Runtime::Native(session) = &self.runtime
            && let Some(operation_id) = self.native_operation(id)
        {
            session.controller.resume(operation_id);
        }
    }

    pub fn approve_relay(&self, cx: &Context<Self>) {
        if let Runtime::Native(session) = &self.runtime
            && let Setup::Running {
                verification_url, ..
            } = &session.model.setup
            && !verification_url.is_empty()
        {
            cx.open_url(verification_url);
        }
    }

    pub fn sharing_setup(&self, theme: Theme, share: &Share, cx: &Context<Self>) -> Div {
        let Runtime::Native(session) = &self.runtime else {
            return column();
        };
        let locale = language(cx);
        let mut form = theme
            .card()
            .w_full()
            .max_w(px(SETUP_WIDTH))
            .p(px(28.))
            .gap(px(20.))
            .child(theme.heading(locale.text(Text::SharingSetup), 24.))
            .child(
                row()
                    .gap(px(12.))
                    .child(theme.icon_tile(share.symbol, Tone::Mint, 40.))
                    .child(theme.label(share.name.clone(), 16.)),
            )
            .child(
                column()
                    .gap(px(8.))
                    .child(theme.caption(locale.text(Text::RelayAddress)))
                    .child(self.setup_form.relay.clone()),
            )
            .child(
                column()
                    .gap(px(8.))
                    .child(theme.caption(locale.text(Text::FrontendAddress)))
                    .child(self.setup_form.frontend.clone()),
            );
        if self
            .native_operation(share.id)
            .and_then(|id| session.model.operations.iter().find(|op| op.id == id))
            .and_then(windshare_host::Operation::share)
            .and_then(|share| share.error.as_ref())
            .is_some_and(|error| error.code == "settings_unavailable")
            && let Some(error) = &share.error
        {
            form = form.child(
                theme
                    .label(error.clone(), 15.)
                    .text_color(Tone::Rose.text(theme)),
            );
        }
        if let Setup::Failed {
            operation_id,
            error,
            ..
        } = &session.model.setup
            && self.native_operation(share.id) == Some(operation_id.as_str())
        {
            form = form.child(
                theme
                    .label(error.message.clone(), 15.)
                    .text_color(Tone::Rose.text(theme)),
            );
        }
        if let Setup::Running {
            verification_url,
            user_code,
            ..
        } = &session.model.setup
            && !verification_url.is_empty()
        {
            form = form.child(
                row()
                    .gap(px(12.))
                    .child(theme.label(user_code.clone(), 18.))
                    .child(Self::button(
                        "approve-device",
                        locale.text(Text::ApproveDevice),
                        Icon::External,
                        ButtonStyle::Secondary,
                        Action::ApproveRelay,
                        cx,
                    )),
            );
        }
        form = form.child(self.setup_actions(share.id, cx));
        column().w_full().items_center().py(px(24.)).child(form)
    }

    fn setup_actions(&self, id: usize, cx: &Context<Self>) -> Div {
        let Runtime::Native(session) = &self.runtime else {
            return row();
        };
        let locale = language(cx);
        let busy = matches!(session.model.setup, Setup::Running { .. });
        let empty = self.setup_form.relay.read(cx).value().trim().is_empty()
            && self.setup_form.frontend.read(cx).value().trim().is_empty();
        row()
            .gap(px(10.))
            .flex_wrap()
            .child(
                Self::button(
                    "save-sharing-setup",
                    locale.text(if busy {
                        Text::ConnectingRelay
                    } else {
                        Text::SaveAndShare
                    }),
                    Icon::Link,
                    ButtonStyle::Primary,
                    Action::SaveSharingSetup(id),
                    cx,
                )
                .disabled(busy || empty || session.quitting),
            )
            .child(
                Self::button(
                    "resume-saved-settings",
                    locale.text(Text::UseSavedSettings),
                    Icon::Refresh,
                    ButtonStyle::Quiet,
                    Action::ResumeShare(id),
                    cx,
                )
                .disabled(busy || session.quitting),
            )
            .child(Self::button(
                "cancel-setup-share",
                locale.text(Text::Cancel),
                Icon::Close,
                ButtonStyle::Quiet,
                Action::ResetShare,
                cx,
            ))
    }
}
