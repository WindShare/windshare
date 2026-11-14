use super::Runtime;
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::{Action, Overlay, ShareSource},
};
use gpui::{Div, Entity, Subscription, Window, prelude::*, px};
use windshare_host::Setup;
use windshare_ui::{ButtonStyle, Icon, Input, InputEvent, Theme, Tone, column, row};

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
                        && let Some(frame) = this.overlays.last()
                        && let Overlay::SharingSetup(id) = frame.content
                    {
                        this.save_sharing_setup(id, cx);
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

    pub fn open_sharing_setup(
        &mut self,
        id: Option<usize>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let id = if let Some(id) = id {
            let finished = self
                .shares
                .iter()
                .any(|share| share.id == id && share.finished());
            if finished {
                let Some(repeated) = self.repeat_native_share(id, window, cx) else {
                    return;
                };
                Some(repeated)
            } else {
                Some(id)
            }
        } else {
            None
        };
        if let Some(share) = id.and_then(|id| self.shares.iter().find(|share| share.id == id))
            && let Some(windshare_host::ShareAuthorization::SelfHosted { relay_url }) =
                &share.authorization
        {
            self.setup_form
                .relay
                .update(cx, |input, cx| input.set_value(relay_url.clone(), cx));
        }
        self.open_overlay(Overlay::SharingSetup(id), window, cx);
    }

    pub fn save_sharing_setup(&self, id: Option<usize>, cx: &Context<Self>) {
        let operation = match id {
            Some(id) => match self.native_operation(id) {
                Some(operation) => operation,
                None => return,
            },
            None => "",
        };
        if let Runtime::Native(session) = &self.runtime
            && !session.model.setup.busy()
            && !session.model.setup.requires_cancellation()
        {
            session.controller.configure(
                operation,
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

    pub fn cancel_sharing_setup(&self) {
        if let Runtime::Native(session) = &self.runtime
            && let Some(context) = session.model.setup.context()
        {
            session.controller.cancel_setup(&context.id);
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

    pub fn sharing_setup(&self, theme: Theme, id: Option<usize>, cx: &Context<Self>) -> Div {
        let Runtime::Native(session) = &self.runtime else {
            return column();
        };
        let locale = language(cx);
        let operation = id.and_then(|id| self.native_operation(id)).unwrap_or("");
        let mut form = column().gap(px(20.));
        if let Some(share) = id.and_then(|id| self.shares.iter().find(|share| share.id == id)) {
            form = form.child(
                row()
                    .gap(px(12.))
                    .child(theme.icon_tile(share.symbol, Tone::Mint, 40.))
                    .child(theme.label(share.name.clone(), 16.)),
            );
        }
        form = form
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
        form = form.child(self.setup_feedback(theme, operation, cx));
        let busy = session.model.setup.busy() || session.model.setup.requires_cancellation();
        let empty = self.setup_form.relay.read(cx).value().trim().is_empty()
            && self.setup_form.frontend.read(cx).value().trim().is_empty();
        form.child(
            row()
                .gap(px(10.))
                .flex_wrap()
                .child(
                    Self::button(
                        "save-sharing-setup",
                        locale.text(if busy {
                            Text::ConnectingRelay
                        } else if id.is_some() {
                            Text::SaveAndShare
                        } else {
                            Text::SaveSettings
                        }),
                        Icon::Link,
                        ButtonStyle::Primary,
                        Action::SaveSharingSetup(id),
                        cx,
                    )
                    .disabled(busy || empty || self.runtime.selection_disabled()),
                )
                .when_some(id, |actions, id| {
                    actions.child(
                        Self::button(
                            "use-saved-sharing-settings",
                            locale.text(Text::UseSavedSettings),
                            Icon::Refresh,
                            ButtonStyle::Secondary,
                            Action::ResumeShare(id),
                            cx,
                        )
                        .disabled(busy || self.runtime.selection_disabled()),
                    )
                })
                .child(Self::button(
                    "close-sharing-setup",
                    locale.text(Text::CloseDialog),
                    Icon::Close,
                    ButtonStyle::Quiet,
                    Action::Close,
                    cx,
                )),
        )
    }
}
