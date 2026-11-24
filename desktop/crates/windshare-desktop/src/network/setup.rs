use super::{MAX_ACTIVE_RELAYS, SetupDialog, SetupDialogEvent, SetupPage, SetupSource};
use crate::{
    desktop::Desktop,
    live::Runtime,
    localization::Text,
    model::{Overlay, Page, SettingsSection, ShareSource, SharingSetupTarget},
};
use gpui::{App, Context, Window, prelude::*};
use windshare_host::{RelayOperation, SavedRelay, Setup, ShareAuthorization};

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

    fn open_setup_dialog(
        &mut self,
        target: SharingSetupTarget,
        page: SetupPage,
        initial_relay_url: Option<String>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> gpui::Entity<SetupDialog> {
        let operation_id = target
            .share_id()
            .and_then(|id| self.native_operation(id))
            .unwrap_or("")
            .to_owned();
        let source = match &self.runtime {
            Runtime::Native(session) => SetupSource::Native {
                host: session.host.clone(),
                operation_id,
            },
            Runtime::Preview(preview) => SetupSource::Preview {
                prefs: preview.preferences.clone(),
            },
        };
        let scope = cx.focus_handle();
        let dialog = cx.new(|cx| {
            SetupDialog::new(
                target,
                source,
                page,
                initial_relay_url,
                scope.clone(),
                window,
                cx,
            )
        });
        let initial_focus = dialog.read(cx).focus_target(cx);
        let subscription = cx.subscribe_in(&dialog, window, |this, dialog, event, window, cx| {
            this.handle_setup_dialog_event(dialog, event.clone(), window, cx);
        });
        let overlay = Overlay::SharingSetup(target, dialog.clone());
        // The host owns a single global setup workflow; navigating to the active
        // setup replaces the current setup modal rather than stacking duplicates.
        if self
            .overlays
            .last()
            .is_some_and(|frame| matches!(frame.content, Overlay::SharingSetup(..)))
        {
            self.replace_top_overlay(
                overlay,
                scope,
                &initial_focus,
                Some(subscription),
                window,
                cx,
            );
        } else {
            self.open_scoped_overlay(
                overlay,
                scope,
                &initial_focus,
                Some(subscription),
                window,
                cx,
            );
        }
        dialog
    }

    fn is_top_setup_dialog(&self, dialog: &gpui::Entity<SetupDialog>) -> bool {
        matches!(
            self.overlays.last().map(|frame| &frame.content),
            Some(Overlay::SharingSetup(_, top)) if top == dialog
        )
    }

    fn handle_setup_dialog_event(
        &mut self,
        dialog: &gpui::Entity<SetupDialog>,
        event: SetupDialogEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if !self.is_top_setup_dialog(dialog) {
            tracing::debug!(
                event = "desktop.setup_dialog_event_ignored",
                session = std::process::id(),
                overlay_depth = self.overlays.len(),
            );
            return;
        }
        match event {
            SetupDialogEvent::PageChanged | SetupDialogEvent::FocusChanged => {
                let focus = dialog.read(cx).focus_target(cx);
                window.focus(&focus, cx);
            }
            SetupDialogEvent::ReturnToRunningSetup => self.return_to_sharing_setup(window, cx),
            SetupDialogEvent::Toast(message) => self.notify(message, cx),
            SetupDialogEvent::Completed(toast) => {
                self.close_overlay(window, cx);
                if let Some(message) = toast {
                    self.notify(message, cx);
                }
            }
            SetupDialogEvent::Close => self.close_overlay(window, cx),
            SetupDialogEvent::PreviewRelay(url) => self.save_preview_relay(url, window, cx),
            SetupDialogEvent::PreviewFrontend(url) => self.save_preview_frontend(url, window, cx),
            SetupDialogEvent::PreviewPreference(edit) => {
                self.apply_preview_preference(edit, cx);
            }
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
            && self.account_view(cx).is_some()
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
        let model = session.model(cx);
        if model.setup_pending() || model.setup.unfinished() {
            return;
        }
        if let Some(Overlay::SharingSetup(_, dialog)) =
            self.overlays.last().map(|frame| &frame.content)
        {
            let dialog = dialog.clone();
            dialog.update(cx, |dialog, cx| {
                dialog.prepare_relay_authorization(relay_url, cx);
            });
            cx.notify();
            return;
        }
        self.network_ui.clear_transient();
        let dialog = self.open_setup_dialog(
            SharingSetupTarget::Defaults,
            SetupPage::Relay(RelayOperation::AuthorizeDevice),
            Some(relay_url),
            window,
            cx,
        );
        dialog.update(cx, SetupDialog::submit);
        cx.notify();
    }

    pub fn open_setup_modal(
        &mut self,
        page: SetupPage,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.network_ui.clear_transient();
        if let Runtime::Native(session) = &self.runtime {
            session.controller(cx).refresh_preferences();
        }
        self.open_setup_dialog(SharingSetupTarget::Defaults, page, None, window, cx);
    }

    pub fn open_sharing_setup(
        &mut self,
        id: Option<usize>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.network_ui.clear_transient();
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        session.controller(cx).refresh_preferences();
        let model = session.model(cx).clone();
        let id = id.or_else(|| {
            let context = model.setup.context().filter(|_| model.setup.unfinished())?;
            self.shares
                .iter()
                .find(|share| {
                    !share.finished()
                        && matches!(&share.source, ShareSource::Native { operation_id } if operation_id == &context.operation_id)
                })
                .map(|share| share.id)
        });
        let operation = id.and_then(|id| self.native_operation(id)).unwrap_or("");
        let current = model.setup.context().filter(|context| {
            model.setup.unfinished() && (operation.is_empty() || context.operation_id == operation)
        });
        let target = id.map_or(SharingSetupTarget::Defaults, |id| {
            if model.waits_for_official_authorization(operation) {
                SharingSetupTarget::SwitchRelay(id)
            } else {
                SharingSetupTarget::RecoverShare(id)
            }
        });
        let (page, relay_url) = if !model.setup_pending()
            && let Some(context) = current
            && let Some(relay_operation) = context.relay_operation
            && !context.relay_url.is_empty()
        {
            (
                SetupPage::Relay(relay_operation),
                Some(context.relay_url.clone()),
            )
        } else {
            (SetupPage::Selection, None)
        };
        self.open_setup_dialog(target, page, relay_url, window, cx);
    }

    pub fn return_to_sharing_setup(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        let setup = &session.model(cx).setup;
        let Some(context) = setup.context().filter(|_| setup.unfinished()).cloned() else {
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

    pub fn resume_native_share(&self, id: usize, cx: &App) {
        if let Runtime::Native(session) = &self.runtime
            && let Some(id) = self.native_operation(id)
        {
            session.controller(cx).resume(id);
        }
    }

    pub fn use_saved_settings(&self, id: usize, cx: &App) {
        if let Runtime::Native(session) = &self.runtime
            && let Some(id) = self.native_operation(id)
        {
            session.controller(cx).use_saved_settings(id);
        }
    }

    pub fn cancel_sharing_setup(&mut self, cx: &mut Context<Self>) {
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        let Some(setup_id) = session.model(cx).setup.context().map(|ctx| ctx.id.clone()) else {
            return;
        };
        let host = session.host.clone();
        self.network_ui.inline_request_id =
            host.update(cx, |host, cx| host.cancel_setup(&setup_id, cx));
    }

    pub fn approve_relay(&self, cx: &Context<Self>) {
        if let Runtime::Native(session) = &self.runtime
            && let Setup::Running {
                verification_url, ..
            } = &session.model(cx).setup
            && !verification_url.is_empty()
        {
            cx.open_url(verification_url);
        }
    }

    pub(crate) fn handoff_setup_dialog(
        &mut self,
        target: SharingSetupTarget,
        dialog: &gpui::Entity<SetupDialog>,
        cx: &App,
    ) {
        if !matches!(target, SharingSetupTarget::Defaults) {
            return;
        }
        let handoff = dialog.read(cx).in_flight_handoff();
        if handoff.is_empty() {
            return;
        }
        let settings_visible =
            self.page == Page::Settings && self.settings_section == SettingsSection::Network;
        // Dismissing a global setup modal does not cancel the host workflow; hand
        // ownership to the settings page so it surfaces the settled outcome once
        // the inline progress banner exits.
        tracing::info!(
            event = "desktop.setup_handoff_to_settings",
            session = std::process::id(),
            setup_id = ?handoff.in_flight_id,
            has_remove_toast = handoff.remove_toast_id.is_some(),
            settings_visible,
        );
        self.network_ui
            .adopt_dialog_handoff(handoff, settings_visible);
    }

    pub(crate) fn observe_active_network_setup(&mut self, cx: &App) {
        if self.page != Page::Settings || self.settings_section != SettingsSection::Network {
            return;
        }
        if let Runtime::Native(session) = &self.runtime {
            self.network_ui.observe_active_setup(session.model(cx));
        }
    }
}
