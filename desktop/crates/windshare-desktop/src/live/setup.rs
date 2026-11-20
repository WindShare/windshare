use super::Runtime;
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::{Overlay, ShareSource, SharingSetupTarget},
};
use gpui::{Entity, Focusable, Subscription, Window, prelude::*};
use std::cell::Cell;
use windshare_host::{Model, PreferenceEdit, RelayOperation, Setup, ShareAuthorization};
use windshare_ui::{Input, InputEvent};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SetupPage {
    Selection,
    Relay(RelayOperation),
    ReceiverSite,
}

impl SetupPage {
    pub const fn title(self) -> Text {
        match self {
            Self::Selection => Text::ChooseRelays,
            Self::Relay(RelayOperation::AddProfile) => Text::ConnectRelay,
            Self::Relay(RelayOperation::AuthorizeDevice) => Text::ApproveDevice,
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
    pub(super) pending_toast: Option<Text>,
    pub(super) feedback: Option<Result<Text, String>>,
    pub(super) rendered_input_ready: Cell<Option<bool>>,
    pub(crate) expanded_relay: Option<String>,
    _subscriptions: Vec<Subscription>,
}

impl SetupForm {
    pub fn new(window: &Window, cx: &mut Context<Desktop>) -> Self {
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
                cx.subscribe_in(input, window, |this, _, event, window, cx| {
                    let Some(target) = this.overlays.last().and_then(|frame| match frame.content {
                        Overlay::SharingSetup(target) => Some(target),
                        _ => None,
                    }) else {
                        return;
                    };
                    match event {
                        InputEvent::Submit => {
                            this.save_sharing_setup(target.share_id(), window, cx);
                            cx.notify();
                        }
                        InputEvent::Changed => {
                            let frontend = &this.relay_preferences_view().frontend;
                            let ready = this.setup_form.input_submittable(frontend, cx);
                            if this.setup_form.rendered_input_ready.get() != Some(ready) {
                                cx.notify();
                            }
                        }
                    }
                })
            })
            .collect();
        Self {
            page: SetupPage::Selection,
            relay,
            access_key,
            frontend,
            pending: None,
            pending_toast: None,
            feedback: None,
            rendered_input_ready: Cell::new(None),
            expanded_relay: None,
            _subscriptions: subscriptions,
        }
    }

    pub(super) fn input_submittable(&self, frontend: &str, cx: &Context<Desktop>) -> bool {
        match self.page {
            SetupPage::Selection => false,
            SetupPage::Relay(_) => !self.relay.read(cx).value().trim().is_empty(),
            SetupPage::ReceiverSite => {
                let url = self.frontend.read(cx).value().trim();
                !url.is_empty() && url != frontend
            }
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

    pub(super) fn reconcile(&mut self, model: &Model, cx: &mut Context<Desktop>) -> Option<Text> {
        if let Some(url) = &self.expanded_relay
            && !model
                .preferences
                .relays
                .iter()
                .any(|relay| &relay.url == url)
        {
            self.expanded_relay = None;
        }
        let id = self.pending.as_ref()?;
        let mut toast = None;
        if let Some(error) = model.settings_rejection(id) {
            self.feedback = Some(Err(error.message.clone()));
            self.pending_toast = None;
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
                    toast = self.pending_toast.take();
                    if context.frontend_saved {
                        Some(Ok(Text::FrontendSaved))
                    } else if context.relay_saved {
                        let enabled = model
                            .preferences
                            .relays
                            .iter()
                            .any(|relay| relay.url == context.relay_url && relay.enabled);
                        Some(Ok(
                            if model.waits_for_official_authorization(&context.operation_id) {
                                Text::RelaySaved
                            } else if enabled {
                                Text::RelayConnected
                            } else {
                                Text::RelaySavedInactive
                            },
                        ))
                    } else {
                        None
                    }
                }
                Setup::Cancelled { .. } => {
                    self.pending_toast = None;
                    self.access_key
                        .update(cx, |input, cx| input.set_value("", cx));
                    Some(Ok(Text::SetupCancelled))
                }
                Setup::Failed { error, .. } if !model.setup.unfinished() => {
                    self.pending_toast = None;
                    Some(Err(error.message.clone()))
                }
                Setup::Failed { .. } | Setup::CancelFailed { .. } => {
                    self.pending_toast = None;
                    None
                }
                _ => return None,
            };
        } else {
            return None;
        }
        self.pending = None;
        toast
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
        if let Some(ShareAuthorization::SelfHosted { relay_url }) = authorization {
            self.authorize_relay(relay_url, window, cx);
        }
    }

    pub(super) fn authorize_relay(
        &mut self,
        relay_url: String,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        if self.setup_form.pending.is_some() || session.model.setup.unfinished() {
            return;
        }
        let id = match self.overlays.last().map(|frame| frame.content.clone()) {
            Some(Overlay::SharingSetup(target)) => target.share_id(),
            _ => None,
        };
        self.edit_setup_page(
            SetupPage::Relay(RelayOperation::AuthorizeDevice),
            window,
            cx,
        );
        self.setup_form
            .relay
            .update(cx, |input, cx| input.set_value(relay_url, cx));
        self.save_sharing_setup(id, window, cx);
        cx.notify();
    }

    pub fn open_setup_modal(
        &mut self,
        page: SetupPage,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.setup_form.expanded_relay = None;
        if let Runtime::Native(session) = &self.runtime {
            session.controller.refresh_preferences();
        }
        self.open_overlay(
            Overlay::SharingSetup(SharingSetupTarget::Defaults),
            window,
            cx,
        );
        self.edit_setup_page(page, window, cx);
    }

    pub fn open_sharing_setup(
        &mut self,
        id: Option<usize>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.setup_form.expanded_relay = None;
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
        let target = id.map_or(SharingSetupTarget::Defaults, |id| {
            if session.model.waits_for_official_authorization(operation) {
                SharingSetupTarget::SwitchRelay(id)
            } else {
                SharingSetupTarget::RecoverShare(id)
            }
        });
        if self.setup_form.pending.is_none() {
            self.setup_form.feedback = None;
            self.setup_form.page = SetupPage::Selection;
            self.setup_form
                .access_key
                .update(cx, |input, cx| input.set_value("", cx));
            if let Some(context) = current
                && let Some(relay_operation) = context.relay_operation
                && !context.relay_url.is_empty()
            {
                self.setup_form.page = SetupPage::Relay(relay_operation);
                self.setup_form.relay.update(cx, |input, cx| {
                    input.set_value(context.relay_url.clone(), cx);
                });
            }
        }
        self.open_overlay(Overlay::SharingSetup(target), window, cx);
    }

    pub fn edit_setup_page(
        &mut self,
        page: SetupPage,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let frontend = match &self.runtime {
            Runtime::Native(session) => {
                if self.setup_form.pending.is_some() || session.model.setup.unfinished() {
                    return;
                }
                session.model.preferences.frontend.clone()
            }
            Runtime::Preview(preview) => preview.preferences.frontend.clone(),
        };
        let form = &mut self.setup_form;
        form.page = page;
        form.feedback = None;
        form.access_key
            .update(cx, |input, cx| input.set_value("", cx));
        let focus = match page {
            SetupPage::Selection => self.overlays.last().map(|frame| frame.scope.clone()),
            SetupPage::Relay(_) => {
                form.relay.update(cx, |input, cx| input.set_value("", cx));
                Some(form.relay.read(cx).focus_handle(cx))
            }
            SetupPage::ReceiverSite => {
                form.frontend
                    .update(cx, |input, cx| input.set_value(frontend, cx));
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

    pub fn save_sharing_setup(
        &mut self,
        id: Option<usize>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if matches!(self.runtime, Runtime::Preview(_)) {
            self.save_preview_setup(window, cx);
            return;
        }
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
            SetupPage::Relay(relay_operation) => {
                let relay = form.relay.read(cx).value().trim();
                if relay.is_empty() {
                    return;
                }
                session.controller.setup_relay(
                    operation,
                    relay_operation,
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
        self.setup_form.pending_toast = None;
        self.setup_form.feedback = None;
    }

    fn save_preview_setup(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let page = self.setup_form.page;
        let relay = self.setup_form.relay.read(cx).value().trim().to_owned();
        let frontend = self.setup_form.frontend.read(cx).value().trim().to_owned();
        let Runtime::Preview(preview) = &mut self.runtime else {
            return;
        };
        let toast = match page {
            SetupPage::Relay(_) if !relay.is_empty() => {
                let enabled = !preview.preferences.at_capacity;
                if let Some(existing) = preview
                    .preferences
                    .relays
                    .iter_mut()
                    .find(|item| item.url == relay)
                {
                    existing.enabled = enabled;
                } else {
                    preview.preferences.relays.push(windshare_host::SavedRelay {
                        url: relay,
                        enabled,
                        console_url: None,
                        offered_frontend: None,
                    });
                }
                preview.preferences.at_capacity = preview
                    .preferences
                    .relays
                    .iter()
                    .filter(|item| item.enabled)
                    .count()
                    >= super::relay_preferences::MAX_ACTIVE_RELAYS;
                if enabled {
                    Text::RelayConnected
                } else {
                    Text::RelaySavedInactive
                }
            }
            SetupPage::ReceiverSite
                if !frontend.is_empty() && frontend != preview.preferences.frontend =>
            {
                preview.preferences.frontend = frontend;
                preview.preferences.custom_frontend = true;
                Text::FrontendSaved
            }
            _ => return,
        };
        self.setup_form
            .access_key
            .update(cx, |input, cx| input.set_value("", cx));
        self.close_overlay(window, cx);
        self.notify(toast, cx);
    }

    pub(super) fn switch_share_relay(
        &mut self,
        id: usize,
        relay_url: &str,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if let Runtime::Native(session) = &self.runtime
            && let Some(id) = self.native_operation(id)
            && session.controller.switch_relay(id, relay_url)
        {
            self.close_overlay(window, cx);
        }
    }

    pub fn resume_native_share(&self, id: usize) {
        if let Runtime::Native(session) = &self.runtime
            && let Some(id) = self.native_operation(id)
        {
            session.controller.resume(id);
        }
    }

    pub fn use_saved_settings(&self, id: usize) {
        if let Runtime::Native(session) = &self.runtime
            && let Some(id) = self.native_operation(id)
        {
            session.controller.use_saved_settings(id);
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
