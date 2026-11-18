use super::Runtime;
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::{Overlay, ShareSource},
};
use gpui::{Entity, Focusable, Subscription, Window, prelude::*};
use windshare_host::{Model, PreferenceEdit, Setup, ShareAuthorization};
use windshare_ui::{Input, InputEvent};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SetupPage {
    Selection,
    ConnectRelay,
    ReceiverSite,
}

impl SetupPage {
    pub const fn title(self) -> Text {
        match self {
            Self::Selection => Text::ChooseRelays,
            Self::ConnectRelay => Text::ConnectRelay,
            Self::ReceiverSite => Text::ReceiverSite,
        }
    }
}

pub struct SetupForm {
    pub page: SetupPage,
    pub(super) relay: Entity<Input>,
    pub(super) access_key: Entity<Input>,
    pub(super) frontend: Entity<Input>,
    pub(super) pending: Option<String>,
    pub(super) feedback: Option<Result<Text, String>>,
    _subscriptions: Vec<Subscription>,
}

impl SetupForm {
    pub fn new(cx: &mut Context<Desktop>) -> Self {
        let locale = language(cx);
        let relay = cx.new(|cx| {
            Input::new(locale.text(Text::RelayAddress), cx).placeholder("https://relay.example")
        });
        let frontend = cx.new(|cx| {
            Input::new(locale.text(Text::ReceiverAddress), cx).placeholder("https://share.example")
        });
        let access_key = cx.new(|cx| Input::new(locale.text(Text::ServiceKey), cx).password());
        let subscriptions = [&relay, &frontend, &access_key]
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
            page: SetupPage::Selection,
            relay,
            access_key,
            frontend,
            pending: None,
            feedback: None,
            _subscriptions: subscriptions,
        }
    }

    pub fn localize(&self, cx: &mut Context<Desktop>) {
        let locale = language(cx);
        for (input, label, placeholder) in [
            (&self.relay, Text::RelayAddress, "https://relay.example"),
            (&self.access_key, Text::ServiceKey, ""),
            (
                &self.frontend,
                Text::ReceiverAddress,
                "https://share.example",
            ),
        ] {
            input.update(cx, |input, cx| {
                input.set_labels(locale.text(label), placeholder, cx);
            });
        }
    }

    pub(super) fn reconcile(&mut self, model: &Model, cx: &mut Context<Desktop>) {
        let Some(id) = &self.pending else { return };
        if let Some(error) = model.settings_rejection(id) {
            self.feedback = Some(Err(error.message.clone()));
        } else if model
            .setup
            .context()
            .is_some_and(|context| &context.id == id)
        {
            self.feedback = match &model.setup {
                Setup::Finished { context } => {
                    self.page = SetupPage::Selection;
                    self.access_key
                        .update(cx, |input, cx| input.set_value("", cx));
                    if context.frontend_saved {
                        Some(Ok(Text::FrontendSaved))
                    } else if context.relay_saved {
                        let enabled = model
                            .preferences
                            .relays
                            .iter()
                            .any(|relay| relay.url == context.relay_url && relay.enabled);
                        Some(Ok(if enabled {
                            Text::RelayConnected
                        } else {
                            Text::RelaySavedInactive
                        }))
                    } else {
                        None
                    }
                }
                Setup::Cancelled { .. } => {
                    self.access_key
                        .update(cx, |input, cx| input.set_value("", cx));
                    Some(Ok(Text::SetupCancelled))
                }
                Setup::Failed { error, .. } if !model.setup.unfinished() => {
                    Some(Err(error.message.clone()))
                }
                Setup::Failed { .. } | Setup::CancelFailed { .. } => None,
                _ => return,
            };
        } else {
            return;
        }
        self.pending = None;
    }
}

impl Desktop {
    pub(super) fn native_operation(&self, id: usize) -> Option<&str> {
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

    pub fn configure_share(&mut self, id: usize, window: &mut Window, cx: &mut Context<Self>) {
        let Some(share) = self.shares.iter().find(|share| share.id == id) else {
            return;
        };
        let authorization = share.authorization.clone();
        let id = if share.finished() {
            let Some(repeated) = self.repeat_native_share(id, window, cx) else {
                return;
            };
            repeated
        } else {
            id
        };
        if matches!(authorization, Some(ShareAuthorization::Official))
            && self.account_view().is_some()
        {
            self.open_account(window, cx);
            return;
        }
        self.open_sharing_setup(Some(id), window, cx);
        if let Some(ShareAuthorization::SelfHosted { relay_url }) = authorization
            && let Runtime::Native(session) = &self.runtime
            && !session.model.setup.unfinished()
        {
            self.edit_setup_page(SetupPage::ConnectRelay, window, cx);
            self.setup_form
                .relay
                .update(cx, |input, cx| input.set_value(relay_url, cx));
        }
    }

    pub fn open_sharing_setup(
        &mut self,
        id: Option<usize>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        session.controller.refresh_preferences();
        let id = id.or_else(|| {
            let context = session.model.setup.context().filter(|_| session.model.setup.unfinished())?;
            self.shares.iter().find(|share| !share.finished()
                && matches!(&share.source, ShareSource::Native { operation_id } if operation_id == &context.operation_id)).map(|share| share.id)
        });
        let operation = id.and_then(|id| self.native_operation(id)).unwrap_or("");
        let current = session.model.setup.context().filter(|context| {
            session.model.setup.unfinished()
                && (operation.is_empty() || context.operation_id == operation)
        });
        if self.setup_form.pending.is_none() {
            self.setup_form.feedback = None;
            self.setup_form.page = SetupPage::Selection;
            self.setup_form
                .access_key
                .update(cx, |input, cx| input.set_value("", cx));
            if let Some(context) = current.filter(|context| !context.relay_url.is_empty()) {
                self.setup_form.page = SetupPage::ConnectRelay;
                self.setup_form.relay.update(cx, |input, cx| {
                    input.set_value(context.relay_url.clone(), cx);
                });
            }
        }
        self.open_overlay(Overlay::SharingSetup(id), window, cx);
    }

    pub fn edit_setup_page(
        &mut self,
        page: SetupPage,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        if self.setup_form.pending.is_some() || session.model.setup.unfinished() {
            return;
        }
        let form = &mut self.setup_form;
        form.page = page;
        form.feedback = None;
        form.access_key
            .update(cx, |input, cx| input.set_value("", cx));
        let focus = match page {
            SetupPage::Selection => self.overlays.last().map(|frame| frame.scope.clone()),
            SetupPage::ConnectRelay => {
                form.relay.update(cx, |input, cx| input.set_value("", cx));
                Some(form.relay.read(cx).focus_handle(cx))
            }
            SetupPage::ReceiverSite => {
                form.frontend.update(cx, |input, cx| {
                    input.set_value(session.model.preferences.frontend.clone(), cx);
                });
                Some(form.frontend.read(cx).focus_handle(cx))
            }
        };
        if let Some(focus) = focus {
            window.focus(&focus, cx);
        }
    }

    pub fn return_to_sharing_setup(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        let setup = &session.model.setup;
        let Some(context) = setup.context().filter(|_| setup.unfinished()) else {
            return;
        };
        let id = self.shares.iter().find_map(|share| {
            (matches!(&share.source, ShareSource::Native { operation_id } if operation_id == &context.operation_id)
                && !share.finished()).then_some(share.id)
        });
        tracing::info!(event = "desktop.setup_returned", setup_id = %context.id, operation_id = %context.operation_id, share_id = ?id);
        self.open_sharing_setup(id, window, cx);
    }

    pub fn save_sharing_setup(&mut self, id: Option<usize>, cx: &Context<Self>) {
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        let operation = match id {
            Some(id) => {
                let Some(op) = self.native_operation(id) else {
                    return;
                };
                op
            }
            None => "",
        };
        let setup = &session.model.setup;
        if self.runtime.selection_disabled()
            || self.setup_form.pending.is_some()
            || !setup.can_configure(operation)
        {
            return;
        }
        let form = &self.setup_form;
        let id = match form.page {
            SetupPage::Selection => return,
            SetupPage::ConnectRelay => {
                let relay = form.relay.read(cx).value().trim();
                if relay.is_empty() {
                    return;
                }
                session.controller.connect_relay(
                    operation,
                    relay.into(),
                    form.access_key.read(cx).value().trim().into(),
                )
            }
            SetupPage::ReceiverSite => {
                let url = form.frontend.read(cx).value().trim();
                if url.is_empty() || url == session.model.preferences.frontend {
                    return;
                }
                session
                    .controller
                    .edit_preference(operation, PreferenceEdit::Frontend(url.into()))
            }
        };
        self.setup_form.pending = id;
        self.setup_form.feedback = None;
    }

    pub fn resume_native_share(&self, id: usize) {
        if let Runtime::Native(session) = &self.runtime
            && let Some(id) = self.native_operation(id)
        {
            session.controller.resume(id);
        }
    }

    pub fn cancel_sharing_setup(&mut self) {
        if let Runtime::Native(session) = &self.runtime
            && let Some(context) = session.model.setup.context()
        {
            self.setup_form.pending = Some(context.id.clone());
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
}
