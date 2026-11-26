use super::{
    SetupDialog, SetupDialogEvent, SetupPage, SetupSource,
    relay_preferences::{prompt_reauthorize_relay, prompt_remove_relay},
    setup_finished_feedback,
};
use crate::{
    live::{HostStore, host_share_running, host_share_state},
    localization::Text,
    model::SharingSetupTarget,
};
use gpui::{App, Context, Entity, Window};
use std::sync::Arc;
use windshare_host::{
    Model, Operation, OperationState, PreferenceEdit, RelayOperation, SelectedRoot, Setup,
    SetupSettlement, ShareState as HostShareState,
};
use windshare_ui::Icon;

pub struct ShareDisplay {
    pub name: String,
    pub symbol: Icon,
    pub finished: bool,
    pub live: bool,
}

pub struct OtherRunningSetup {
    pub share_name: Option<String>,
}

impl SetupDialog {
    pub fn submit(&mut self, cx: &mut Context<Self>) {
        if !self.is_preview()
            && (self.selection_disabled(cx)
                || self.pending(cx)
                || self.share_finished(cx)
                || !self.setup(cx).can_configure(self.operation_id()))
        {
            return;
        }
        match self.page {
            SetupPage::Selection => {}
            SetupPage::Relay(relay_operation) => {
                let relay_url = self.relay.read(cx).value().trim().to_owned();
                if relay_url.is_empty() {
                    return;
                }
                if self.is_preview() {
                    cx.emit(SetupDialogEvent::PreviewRelay(relay_url));
                    return;
                }
                let access_key = self.access_key.read(cx).value().trim().to_owned();
                if let SetupSource::Native { host, operation_id } = &self.source {
                    let host = host.clone();
                    let operation_id = operation_id.clone();
                    self.feedback = None;
                    self.in_flight_id = Some(host.update(cx, |host, cx| {
                        host.submit_relay_setup(
                            &operation_id,
                            relay_operation,
                            relay_url,
                            access_key,
                            cx,
                        )
                    }));
                }
            }
            SetupPage::ReceiverSite => {
                let url = self.frontend.read(cx).value().trim().to_owned();
                if url.is_empty() || url == self.prefs(cx).frontend {
                    return;
                }
                if self.is_preview() {
                    cx.emit(SetupDialogEvent::PreviewFrontend(url));
                    return;
                }
                self.apply_preference(PreferenceEdit::Frontend(url), cx);
            }
        }
        cx.notify();
    }

    pub(super) fn apply_preference(&mut self, edit: PreferenceEdit, cx: &mut Context<Self>) {
        if self.is_preview() {
            cx.emit(SetupDialogEvent::PreviewPreference(edit));
            return;
        }
        if self.pending(cx)
            || self.setup(cx).unfinished()
            || self.selection_disabled(cx)
            || self.share_finished(cx)
        {
            return;
        }
        let SetupSource::Native { host, operation_id } = &self.source else {
            return;
        };
        let host = host.clone();
        let operation_id = operation_id.clone();
        self.feedback = None;
        self.in_flight_id = Some(host.update(cx, |host, cx| {
            host.submit_preference(&operation_id, edit, cx)
        }));
        cx.notify();
    }

    pub(super) fn switch_share_relay(&self, relay_url: &str, cx: &mut Context<Self>) {
        if self.share_finished(cx) {
            return;
        }
        if let SetupSource::Native { host, operation_id } = &self.source
            && !operation_id.is_empty()
            && host
                .read(cx)
                .controller()
                .switch_relay(operation_id, relay_url)
        {
            cx.emit(SetupDialogEvent::Close);
        }
    }

    pub(super) fn use_saved_settings(&self, cx: &App) {
        if self.share_finished(cx) {
            return;
        }
        if let SetupSource::Native { host, operation_id } = &self.source
            && !operation_id.is_empty()
        {
            host.read(cx).controller().use_saved_settings(operation_id);
        }
    }

    pub(super) fn cancel_setup(&mut self, cx: &mut Context<Self>) {
        let SetupSource::Native { host, .. } = &self.source else {
            return;
        };
        let Some(setup_id) = host
            .read(cx)
            .model()
            .setup
            .context()
            .map(|ctx| ctx.id.clone())
        else {
            return;
        };
        let host = host.clone();
        if host.update(cx, |host, cx| host.cancel_setup(&setup_id, cx)) {
            self.in_flight_id = Some(setup_id);
        }
        cx.notify();
    }

    pub(super) fn approve_relay(&self, cx: &App) {
        if let Setup::Running {
            verification_url, ..
        } = self.setup(cx)
            && !verification_url.is_empty()
        {
            cx.open_url(verification_url);
        }
    }

    pub(super) fn confirm_reauthorize_relay(
        &self,
        relay_url: String,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if self.busy(cx) || self.is_preview() {
            return;
        }
        let confirmed = prompt_reauthorize_relay(&relay_url, window, cx);
        cx.spawn_in(window, async move |view, cx| {
            if !confirmed.await {
                return;
            }
            let _ = view.update_in(cx, |this, _, cx| {
                if this.busy(cx)
                    || !this
                        .prefs(cx)
                        .relays
                        .iter()
                        .any(|relay| relay.url == relay_url)
                {
                    return;
                }
                this.expanded_relay = None;
                tracing::info!(
                    event = "desktop.relay_reauthorize_confirmed",
                    session = std::process::id()
                );
                this.prepare_relay_authorization(relay_url, cx);
            });
        })
        .detach();
    }

    pub(super) fn confirm_remove_relay(
        &self,
        relay_url: String,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if self.busy(cx) {
            return;
        }
        let confirmed = prompt_remove_relay(&relay_url, window, cx);
        cx.spawn_in(window, async move |view, cx| {
            if !confirmed.await {
                return;
            }
            let _ = view.update_in(cx, |this, _, cx| {
                if this.busy(cx)
                    || !this
                        .prefs(cx)
                        .relays
                        .iter()
                        .any(|relay| relay.url == relay_url)
                {
                    return;
                }
                this.expanded_relay = None;
                tracing::info!(
                    event = "desktop.relay_remove_confirmed",
                    session = std::process::id()
                );
                this.apply_preference(PreferenceEdit::Remove(relay_url), cx);
            });
        })
        .detach();
    }

    pub(super) fn on_host_updated(&mut self, host: &Entity<HostStore>, cx: &mut Context<Self>) {
        let (previous_model, model) = {
            let store = host.read(cx);
            (
                Arc::clone(store.previous_model()),
                Arc::clone(store.model()),
            )
        };
        if let Some(url) = &self.expanded_relay
            && !model
                .preferences
                .relays
                .iter()
                .any(|relay| &relay.url == url)
        {
            self.expanded_relay = None;
        }
        let op_id = self.operation_id().to_owned();
        let previous_page = self.page;
        let previous_form_visible = form_visible_for(&previous_model, &op_id);
        let finished = self.settle_in_flight(&model, cx);
        match dialog_close_action(self.target, &previous_model, &model, &op_id, finished) {
            DialogCloseAction::CompleteWithToast => {
                let toast = self.feedback.take().and_then(Result::ok);
                cx.emit(SetupDialogEvent::Completed(toast));
                return;
            }
            DialogCloseAction::CloseWithoutToast => {
                tracing::info!(
                    event = "desktop.setup_dialog_closed_for_finished_share",
                    session = std::process::id(),
                    operation_id = %op_id,
                );
                cx.emit(SetupDialogEvent::Close);
                return;
            }
            DialogCloseAction::StayOpen => {}
        }
        if self.page != previous_page {
            cx.emit(SetupDialogEvent::PageChanged);
        } else if form_visible_for(&model, &op_id) != previous_form_visible {
            cx.emit(SetupDialogEvent::FocusChanged);
        }
        cx.notify();
    }

    fn settle_in_flight(&mut self, model: &Model, cx: &mut Context<Self>) -> bool {
        let Some(tracked) = self.in_flight_id.as_deref() else {
            return false;
        };
        match model.setup_settlement(tracked) {
            Some(SetupSettlement::Finished) => {
                self.in_flight_id = None;
                self.clear_access_key(cx);
                self.page = SetupPage::Selection;
                self.feedback = setup_finished_feedback(model).map(Ok);
                true
            }
            Some(SetupSettlement::Cancelled) => {
                self.in_flight_id = None;
                self.clear_access_key(cx);
                self.feedback = Some(Ok(Text::SetupCancelled));
                false
            }
            Some(SetupSettlement::Failed(error)) => {
                self.in_flight_id = None;
                self.feedback = (!model.setup.unfinished()).then(|| Err(error.message.clone()));
                false
            }
            None => false,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum DialogCloseAction {
    StayOpen,
    CompleteWithToast,
    CloseWithoutToast,
}

pub(super) fn operation_finished(model: &Model, operation_id: &str) -> bool {
    !operation_id.is_empty()
        && model
            .operations
            .iter()
            .find(|op| op.id == operation_id)
            .is_some_and(|op| match &op.state {
                OperationState::Submitting => false,
                OperationState::Rejected(_) => true,
                OperationState::Accepted(share) => share.is_finished(),
            })
}

pub(super) fn dialog_close_action(
    target: SharingSetupTarget,
    previous: &Model,
    current: &Model,
    operation_id: &str,
    setup_finished: bool,
) -> DialogCloseAction {
    match target {
        SharingSetupTarget::Defaults => {
            if setup_finished {
                DialogCloseAction::CompleteWithToast
            } else {
                DialogCloseAction::StayOpen
            }
        }
        SharingSetupTarget::RecoverShare(_) | SharingSetupTarget::SwitchRelay(_) => {
            if operation_finished(current, operation_id) {
                // A share-scoped modal cannot configure a terminal operation; close
                // without a setup toast so the underlying share outcome is visible.
                return DialogCloseAction::CloseWithoutToast;
            }
            let prev = host_share_state(previous, operation_id);
            let next = host_share_state(current, operation_id);
            let auth_completed = matches!(target, SharingSetupTarget::RecoverShare(_))
                && matches!(prev, Some(HostShareState::AwaitingAuthorization { .. }))
                && setup_finished
                && matches!(
                    &current.setup,
                    Setup::Finished { context }
                        if context.operation_id == operation_id
                            && context.relay_operation == Some(RelayOperation::AuthorizeDevice)
                            && context.relay_saved
                );
            let share_unblocked = prev.is_some_and(|s| !host_share_running(s))
                && next.is_some_and(host_share_running);
            if auth_completed || share_unblocked {
                DialogCloseAction::CompleteWithToast
            } else {
                DialogCloseAction::StayOpen
            }
        }
    }
}

pub(super) fn setup_shows_progress(setup: &Setup) -> bool {
    let connecting = setup.context().is_some_and(|ctx| !ctx.relay_url.is_empty());
    setup.busy() && connecting || setup.requires_cancellation()
}

pub(super) fn other_running_for_model(
    model: &Model,
    operation_id: &str,
) -> Option<OtherRunningSetup> {
    let setup = &model.setup;
    setup
        .context()
        .filter(|ctx| {
            setup.unfinished() && !operation_id.is_empty() && ctx.operation_id != operation_id
        })
        .map(|ctx| OtherRunningSetup {
            share_name: model
                .operations
                .iter()
                .find(|op| op.id == ctx.operation_id)
                .map(operation_name),
        })
}

fn form_visible_for(model: &Model, operation_id: &str) -> bool {
    other_running_for_model(model, operation_id).is_none() && !setup_shows_progress(&model.setup)
}

fn operation_name(operation: &Operation) -> String {
    if let Some(root) = operation
        .share()
        .and_then(|share| share.selection.as_ref())
        .and_then(|selection| selection.root.as_ref())
    {
        return match root {
            SelectedRoot::File { name, .. } | SelectedRoot::Directory { name } => name.clone(),
        };
    }
    let name = operation.paths.first().map_or_else(String::new, |path| {
        path.file_name()
            .unwrap_or(path.as_os_str())
            .to_string_lossy()
            .into_owned()
    });
    let count = operation.paths.len();
    if count > 1 {
        format!("{name} + {}", count - 1)
    } else {
        name
    }
}

pub(super) fn operation_display(operation: &Operation) -> ShareDisplay {
    let symbol = match operation
        .share()
        .and_then(|share| share.selection.as_ref())
        .and_then(|selection| selection.root.as_ref())
    {
        Some(SelectedRoot::File { .. }) => Icon::File,
        _ => Icon::Folder,
    };
    let (finished, live) = match &operation.state {
        OperationState::Submitting => (false, false),
        OperationState::Rejected(_) => (true, false),
        OperationState::Accepted(share) => (
            share.is_finished(),
            matches!(share.state, HostShareState::Sharing { .. }),
        ),
    };
    ShareDisplay {
        name: operation_name(operation),
        symbol,
        finished,
        live,
    }
}
