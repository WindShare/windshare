use super::{
    MAX_ACTIVE_RELAYS, OtherRunningSetup, SetupDialog, SetupDialogEvent, SetupDialogSnapshot,
    SetupPage, SetupRuntimeMode, SetupShareSummary,
};
use crate::{
    desktop::Desktop,
    live::Runtime,
    localization::Text,
    model::{Overlay, ShareSource, SharingSetupTarget},
};
use gpui::{Context, Window, prelude::*};
use windshare_host::{PreferenceEdit, RelayOperation, SavedRelay, Setup, ShareAuthorization};

impl Desktop {
    pub(crate) fn native_operation(&self, id: usize) -> Option<&str> {
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

    pub(crate) fn build_setup_snapshot(&self, target: SharingSetupTarget) -> SetupDialogSnapshot {
        let id = target.share_id();
        let operation = id.and_then(|id| self.native_operation(id)).unwrap_or("");
        let (setup, command_error, waits_official, other_running) = match &self.runtime {
            Runtime::Native(session) => {
                let setup = &session.model.setup;
                let other = setup
                    .context()
                    .filter(|ctx| {
                        setup.unfinished()
                            && !operation.is_empty()
                            && ctx.operation_id != operation
                    })
                    .map(|ctx| OtherRunningSetup {
                        share_name: self
                            .shares
                            .iter()
                            .find(|share| {
                                matches!(&share.source, ShareSource::Native { operation_id } if operation_id == &ctx.operation_id)
                            })
                            .map(|share| share.name.clone()),
                    });
                (
                    setup.clone(),
                    session.model.command_error.clone(),
                    session.model.waits_for_official_authorization(operation),
                    other,
                )
            }
            Runtime::Preview(_) => (Setup::Idle, None, false, None),
        };
        let share = id
            .and_then(|id| self.shares.iter().find(|share| share.id == id))
            .map(|share| SetupShareSummary {
                id: share.id,
                name: share.name.clone(),
                symbol: share.symbol,
                finished: share.finished(),
                live: share.live(),
                operation_id: operation.to_owned(),
            });
        let mode = if self.runtime.is_preview() {
            SetupRuntimeMode::Preview
        } else {
            SetupRuntimeMode::Native {
                waits_official,
                selection_disabled: self.runtime.selection_disabled(),
                pending: self.setup.pending.is_some(),
            }
        };
        SetupDialogSnapshot {
            target,
            prefs: self.relay_preferences_view().clone(),
            setup,
            command_error,
            mode,
            share,
            other_running,
            feedback: self.setup.feedback.clone(),
            expanded_relay: self.setup.expanded_relay.clone(),
        }
    }

    pub(super) fn sync_open_setup_dialog(&self, window: &mut Window, cx: &mut Context<Self>) {
        if let Some(Overlay::SharingSetup(target, dialog)) =
            self.overlays.last().map(|frame| &frame.content)
        {
            let snapshot = self.build_setup_snapshot(*target);
            dialog.update(cx, |dialog, cx| {
                dialog.sync_snapshot(snapshot, None, window, cx);
            });
        }
    }

    fn open_setup_dialog(
        &mut self,
        target: SharingSetupTarget,
        page: SetupPage,
        initial_relay_url: Option<String>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let snapshot = self.build_setup_snapshot(target);
        let scope = cx.focus_handle();
        let dialog = cx.new(|cx| {
            SetupDialog::new(snapshot, page, initial_relay_url, scope.clone(), window, cx)
        });
        let initial_focus = dialog.read(cx).focus_target(cx);
        let subscription = cx.subscribe_in(&dialog, window, |this, _, event, window, cx| {
            this.handle_setup_dialog_event(event.clone(), window, cx);
        });
        self.open_scoped_overlay(
            Overlay::SharingSetup(target, dialog),
            scope,
            &initial_focus,
            Some(subscription),
            window,
            cx,
        );
    }

    fn handle_setup_dialog_event(
        &mut self,
        event: SetupDialogEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        match event {
            SetupDialogEvent::PageChanged => {}
            SetupDialogEvent::SubmitRelay {
                share_id,
                relay_operation,
                relay_url,
                access_key,
            } => {
                self.submit_relay_setup(
                    share_id,
                    relay_operation,
                    relay_url,
                    access_key,
                    window,
                    cx,
                );
            }
            SetupDialogEvent::SubmitFrontend { share_id, url } => {
                self.submit_frontend_setup(share_id, url, window, cx);
            }
            SetupDialogEvent::EditPreference(edit) => self.apply_preference(edit, window, cx),
            SetupDialogEvent::ToggleRelayActions(url) => {
                self.toggle_relay_actions(url, window, cx);
            }
            SetupDialogEvent::SwitchShareRelay {
                share_id,
                relay_url,
            } => self.switch_share_relay(share_id, &relay_url, window, cx),
            SetupDialogEvent::UseSavedSettings(id) => self.use_saved_settings(id),
            SetupDialogEvent::ReturnToRunningSetup => self.return_to_sharing_setup(window, cx),
            SetupDialogEvent::CancelSetup => {
                self.cancel_sharing_setup();
                self.sync_open_setup_dialog(window, cx);
            }
            SetupDialogEvent::ApproveRelay => self.approve_relay(cx),
            SetupDialogEvent::ConfirmReauthorizeRelay(url) => {
                self.confirm_reauthorize_relay(url, window, cx);
            }
            SetupDialogEvent::ConfirmRemoveRelay(url) => {
                self.confirm_remove_relay(url, window, cx);
            }
            SetupDialogEvent::Close => self.close_overlay(window, cx),
        }
        cx.notify();
    }

    pub fn configure_share(&mut self, id: usize, window: &mut Window, cx: &mut Context<Self>) {
        let Some(share) = self.shares.iter().find(|share| share.id == id) else {
            return;
        };
        let authorization = share.authorization().cloned();
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

    pub fn authorize_relay(
        &mut self,
        relay_url: String,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        if self.setup.pending.is_some() || session.model.setup.unfinished() {
            return;
        }
        if let Some(Overlay::SharingSetup(_, dialog)) =
            self.overlays.last().map(|frame| &frame.content)
        {
            let dialog = dialog.clone();
            dialog.update(cx, |dialog, cx| {
                dialog.prepare_relay_authorization(relay_url, window, cx);
            });
            cx.notify();
            return;
        }
        self.open_setup_dialog(
            SharingSetupTarget::Defaults,
            SetupPage::Relay(RelayOperation::AuthorizeDevice),
            Some(relay_url.clone()),
            window,
            cx,
        );
        self.submit_relay_setup(
            None,
            RelayOperation::AuthorizeDevice,
            relay_url,
            String::new(),
            window,
            cx,
        );
        cx.notify();
    }

    pub fn open_setup_modal(
        &mut self,
        page: SetupPage,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.setup.expanded_relay = None;
        self.setup.feedback = None;
        if let Runtime::Native(session) = &self.runtime {
            session.controller.refresh_preferences();
        }
        self.open_setup_dialog(SharingSetupTarget::Defaults, page, None, window, cx);
    }

    pub fn open_sharing_setup(
        &mut self,
        id: Option<usize>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.setup.expanded_relay = None;
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        session.controller.refresh_preferences();
        let id = id.or_else(|| {
            let context = session
                .model
                .setup
                .context()
                .filter(|_| session.model.setup.unfinished())?;
            self.shares
                .iter()
                .find(|share| {
                    !share.finished()
                        && matches!(&share.source, ShareSource::Native { operation_id } if operation_id == &context.operation_id)
                })
                .map(|share| share.id)
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
        let (page, relay_url) = if self.setup.pending.is_none() {
            self.setup.feedback = None;
            if let Some(context) = current
                && let Some(relay_operation) = context.relay_operation
                && !context.relay_url.is_empty()
            {
                (
                    SetupPage::Relay(relay_operation),
                    Some(context.relay_url.clone()),
                )
            } else {
                (SetupPage::Selection, None)
            }
        } else {
            (SetupPage::Selection, None)
        };
        self.open_setup_dialog(target, page, relay_url, window, cx);
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
                && !share.finished())
                .then_some(share.id)
        });
        tracing::info!(
            event = "desktop.setup_returned",
            setup_id = %context.id,
            operation_id = %context.operation_id,
            share_id = ?id
        );
        self.open_sharing_setup(id, window, cx);
    }

    fn submit_relay_setup(
        &mut self,
        id: Option<usize>,
        relay_operation: RelayOperation,
        relay_url: String,
        access_key: String,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if matches!(self.runtime, Runtime::Preview(_)) {
            self.save_preview_relay(relay_url, window, cx);
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
        if self.runtime.selection_disabled()
            || self.setup.pending.is_some()
            || !session.model.setup.can_configure(operation)
        {
            return;
        }
        self.setup.pending =
            session
                .controller
                .setup_relay(operation, relay_operation, relay_url, access_key);
        self.setup.pending_toast = None;
        self.setup.feedback = None;
        self.sync_open_setup_dialog(window, cx);
    }

    fn submit_frontend_setup(
        &mut self,
        id: Option<usize>,
        url: String,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if matches!(self.runtime, Runtime::Preview(_)) {
            self.save_preview_frontend(url, window, cx);
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
        if self.runtime.selection_disabled()
            || self.setup.pending.is_some()
            || !session.model.setup.can_configure(operation)
            || url == session.model.preferences.frontend
        {
            return;
        }
        self.setup.pending = session
            .controller
            .edit_preference(operation, PreferenceEdit::Frontend(url));
        self.setup.pending_toast = None;
        self.setup.feedback = None;
        self.sync_open_setup_dialog(window, cx);
    }

    fn save_preview_relay(&mut self, relay: String, window: &mut Window, cx: &mut Context<Self>) {
        let Runtime::Preview(preview) = &mut self.runtime else {
            return;
        };
        let enabled = !preview.preferences.at_capacity;
        if let Some(existing) = preview
            .preferences
            .relays
            .iter_mut()
            .find(|item| item.url == relay)
        {
            existing.enabled = enabled;
        } else {
            preview.preferences.relays.push(SavedRelay {
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
            >= MAX_ACTIVE_RELAYS;
        let toast = if enabled {
            Text::RelayConnected
        } else {
            Text::RelaySavedInactive
        };
        if let Some(Overlay::SharingSetup(_, dialog)) =
            self.overlays.last().map(|frame| &frame.content)
        {
            dialog.update(cx, |dialog, cx| dialog.clear_access_key(cx));
        }
        self.close_overlay(window, cx);
        self.notify(toast, cx);
    }

    fn save_preview_frontend(
        &mut self,
        frontend: String,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Runtime::Preview(preview) = &mut self.runtime else {
            return;
        };
        if frontend == preview.preferences.frontend {
            return;
        }
        preview.preferences.frontend = frontend;
        preview.preferences.custom_frontend = true;
        self.close_overlay(window, cx);
        self.notify(Text::FrontendSaved, cx);
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
            self.setup.pending = Some(context.id.clone());
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
