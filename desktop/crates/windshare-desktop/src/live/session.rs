use crate::{
    desktop::Desktop,
    flow_state::ShareRun,
    localization::Text,
    model::{Overlay, Page, ShareSource, SharingSetupTarget},
};
use gpui::{Context, Window};
use std::{
    collections::HashMap,
    sync::Arc,
    time::{Duration, SystemTime},
};
use windshare_host::{
    AccountProfile, AccountState, AccountStatus, AccountView, Allowance, Connection, Controller,
    Model, Nickname, RelayMode, RelayOperation, RelayPreferences, SavedRelay,
    ShareState as HostShareState,
};

pub const PREVIEW_DEFAULT_FRONTEND: &str = "https://windshare.top";
const PREVIEW_REMAINING_BYTES: u64 = 1_932_735_283;

pub struct PreviewRuntime {
    pub delay: Duration,
    pub preferences: RelayPreferences,
    pub account: AccountView,
}

#[derive(Clone, Copy)]
enum OverlayCompletion {
    Keep,
    Close,
    CloseWithToast(Text),
}

struct OverlayReconcile<'a> {
    previous_model: &'a Model,
    model: &'a Model,
    previous_page: super::SetupPage,
    had_setup_pending: bool,
    previous_account_page: super::AccountPage,
    previous_field: Option<&'a gpui::FocusHandle>,
}

fn share_operation_id(shares: &[crate::model::Share], id: usize) -> Option<&str> {
    shares
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

fn host_share_state(model: &Model, operation_id: &str) -> Option<HostShareState> {
    model
        .operations
        .iter()
        .find(|op| op.id == operation_id)
        .and_then(windshare_host::Operation::share)
        .map(|share| share.state)
}

const fn host_share_running(state: HostShareState) -> bool {
    matches!(state, HostShareState::Preparing | HostShareState::Sharing)
}

pub enum Runtime {
    Preview(Box<PreviewRuntime>),
    Native(Box<Session>),
}

#[derive(Clone, Copy, PartialEq, Eq)]
pub enum QuitState {
    Running,
    Confirming,
    Exiting,
}

pub struct Session {
    pub controller: Controller,
    pub model: Arc<Model>,
    pub picking: bool,
    #[cfg(windows)]
    pub picker: Option<windshare_activation::windows::picker::Picker>,
    pub interaction_epoch: u64,
    pub application: super::application::Application,
    pub presentations: std::collections::HashMap<String, ShareRun>,
    pub quit_state: QuitState,
    pub error: Option<String>,
    retry_wakeup: Option<gpui::Task<()>>,
}

impl Runtime {
    pub fn preview(delay: Duration) -> Self {
        Self::Preview(Box::new(PreviewRuntime {
            delay,
            preferences: RelayPreferences {
                mode: RelayMode::Automatic,
                frontend: PREVIEW_DEFAULT_FRONTEND.into(),
                custom_frontend: false,
                relays: vec![
                    SavedRelay {
                        url: "https://relay-hk.example.com".into(),
                        enabled: true,
                        console_url: Some("https://console.relay-hk.example.com".into()),
                        offered_frontend: Some("https://share.relay-hk.example.com".into()),
                    },
                    SavedRelay {
                        url: "https://relay-tokyo.example.net".into(),
                        enabled: false,
                        console_url: Some("https://console.relay-tokyo.example.net".into()),
                        offered_frontend: None,
                    },
                ],
                at_capacity: false,
            },
            account: AccountView {
                status: AccountStatus {
                    active: true,
                    state: AccountState::Ready,
                    email_code_pending: false,
                    account: "k9M4pQ2xL7vN8rT3wY6zBa".into(),
                    credential: "dR5tY8uI2oP4aS7fG9hJkL".into(),
                    username: "alex_dev".into(),
                    profile: AccountProfile {
                        nickname: Some(Nickname {
                            value: "Alex".into(),
                            next_change_at: 0,
                        }),
                        email: "alex@example.com".into(),
                    },
                    pending_email: None,
                    allowance: Allowance::Available,
                    remaining_bytes: PREVIEW_REMAINING_BYTES,
                    reset_at: String::new(),
                    reason: String::new(),
                },
                reset_local: "2026-11-21 08:00 CST".into(),
                nickname_change_local: String::new(),
                operation: None,
            },
        }))
    }

    pub fn native(controller: Controller, application: super::application::Application) -> Self {
        Self::Native(Box::new(Session {
            model: controller.model(),
            controller,
            picking: false,
            #[cfg(windows)]
            picker: None,
            interaction_epoch: 0,
            application,
            presentations: std::collections::HashMap::new(),
            quit_state: QuitState::Running,
            error: None,
            retry_wakeup: None,
        }))
    }

    pub const fn is_preview(&self) -> bool {
        matches!(self, Self::Preview(_))
    }

    pub fn selection_disabled(&self) -> bool {
        match self {
            Self::Preview(_) => false,
            Self::Native(session) => {
                matches!(session.quit_state, QuitState::Exiting)
                    || matches!(
                        session.model.connection,
                        Connection::Closing | Connection::Closed(_)
                    )
            }
        }
    }

    pub fn notice(&self) -> Option<String> {
        let Self::Native(session) = self else {
            return None;
        };
        session
            .error
            .clone()
            .or_else(|| session.model.command_error.clone())
            .or_else(|| {
                if let Connection::Closed(Err(error)) = &session.model.connection {
                    Some(error.clone())
                } else {
                    None
                }
            })
    }

    pub fn setup_cancellation_error(&self) -> Option<&str> {
        if let Self::Native(session) = self
            && let windshare_host::Setup::CancelFailed { error, .. } = &session.model.setup
        {
            Some(&error.message)
        } else {
            None
        }
    }

    pub fn status(&self) -> Text {
        let Self::Native(session) = self else {
            return Text::PreviewStatus;
        };
        match session.model.connection {
            Connection::Starting => Text::StartingHost,
            Connection::Connected => Text::ReadyToShare,
            Connection::Reconnecting(_) => Text::ControlsReconnecting,
            Connection::Closing => Text::ClosingShares,
            Connection::Closed(_) => Text::HostClosed,
        }
    }
}

impl Desktop {
    pub(super) const fn interaction_epoch(&self) -> u64 {
        match &self.runtime {
            Runtime::Native(session) => session.interaction_epoch,
            Runtime::Preview(_) => 0,
        }
    }

    pub const fn page_available(&self, page: Page) -> bool {
        self.runtime.is_preview() || matches!(page, Page::Share | Page::Shares | Page::Settings)
    }

    pub fn observe_runtime(controller: Controller, window: &Window, cx: &Context<Self>) {
        let weak = cx.entity().downgrade();
        window.on_window_should_close(cx, move |window, cx| {
            let _ = weak.update(cx, |this, cx| this.request_quit(window, cx));
            false
        });
        cx.spawn_in(window, async move |view, cx| {
            loop {
                controller.changed().await;
                if view.update_in(cx, Self::refresh_native).is_err() {
                    break;
                }
            }
        })
        .detach();
    }

    fn refresh_native(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Runtime::Native(session) = &mut self.runtime else {
            return;
        };
        let model = session.controller.model();
        if Arc::ptr_eq(&model, &session.model) {
            return;
        }
        let previous_page = self.setup_form.page;
        let previous_setup_pending = self.setup_form.pending.clone();
        let previous_account_page = self.account_form.page();
        let previous_field = self.account_form.field_focus(&session.model, cx);
        let completed_toast = self.setup_form.reconcile(&model, cx);
        self.account_form.reconcile(&model, cx);
        let close_completed = Self::reconcile_settings_overlay(
            &self.overlays,
            &self.shares,
            self.page,
            self.share_flow,
            &mut self.setup_form,
            &mut self.account_form,
            &OverlayReconcile {
                previous_model: &session.model,
                model: &model,
                previous_page,
                had_setup_pending: previous_setup_pending.is_some(),
                previous_account_page,
                previous_field: previous_field.as_ref(),
            },
            window,
            cx,
        );
        if !self.shares.is_empty() && !model.operations.is_empty() {
            let operations: HashMap<&str, &windshare_host::Operation> = model
                .operations
                .iter()
                .map(|op| (op.id.as_str(), op))
                .collect();
            for share in &mut self.shares {
                let ShareSource::Native { operation_id } = &share.source else {
                    continue;
                };
                if let Some(operation) = operations.get(operation_id.as_str()) {
                    let was_ready = share.live_publication().is_some();
                    super::projection::update_share(share, operation);
                    if !was_ready && share.live_publication().is_some() {
                        tracing::info!(event = "share.ready", operation = %operation.id,
                            share_id = share.id, preview = false);
                    }
                }
            }
        }
        session.retry_wakeup = Self::schedule_retry_wakeups(&model, cx);
        if let windshare_host::Setup::Running {
            verification_url,
            service_login: None,
            ..
        } = &model.setup
            && !verification_url.is_empty()
            && !matches!(&session.model.setup, windshare_host::Setup::Running { verification_url: previous, .. } if previous == verification_url)
        {
            cx.open_url(verification_url);
        }
        session.model = model;
        match close_completed {
            OverlayCompletion::Keep => {
                if let Some(text) = completed_toast {
                    self.notify(text, cx);
                }
            }
            OverlayCompletion::Close => {
                self.close_overlay(window, cx);
                if let Some(text) = completed_toast {
                    self.notify(text, cx);
                }
            }
            OverlayCompletion::CloseWithToast(text) => {
                self.close_overlay(window, cx);
                self.notify(text, cx);
            }
        }
        cx.notify();
    }

    fn schedule_retry_wakeups(model: &Model, cx: &Context<Self>) -> Option<gpui::Task<()>> {
        // This timer only re-enables presentation at the host's deadline.
        // It never refreshes the directory or retries an operation.
        let mut deadlines: Vec<_> = model
            .operations
            .iter()
            .filter_map(|op| op.share())
            .filter_map(|share| match share.state {
                windshare_host::ShareState::AwaitingNetwork { blocking } => {
                    super::blocking::retry_deadline(blocking)
                }
                _ => None,
            })
            .chain(
                model
                    .account
                    .as_ref()
                    .and_then(windshare_host::AccountView::nickname_deadline),
            )
            .filter(|at| *at > SystemTime::now())
            .collect();
        deadlines.sort_unstable();
        deadlines.dedup();
        (!deadlines.is_empty()).then(|| {
            cx.spawn(async move |view, cx| {
                for at in deadlines {
                    let delay = at.duration_since(SystemTime::now()).unwrap_or_default();
                    cx.background_executor().timer(delay).await;
                    if view.update(cx, |_, cx| cx.notify()).is_err() {
                        break;
                    }
                }
            })
        })
    }

    #[allow(clippy::too_many_arguments)]
    fn reconcile_settings_overlay(
        overlays: &[crate::overlays::OverlayFrame],
        shares: &[crate::model::Share],
        page: Page,
        share_flow: crate::flow_state::ShareFlow,
        setup_form: &mut super::SetupForm,
        account_form: &mut super::AccountForm,
        state: &OverlayReconcile<'_>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> OverlayCompletion {
        let Some(frame) = overlays.last() else {
            return OverlayCompletion::Keep;
        };
        match frame.content {
            Overlay::Account => Self::reconcile_account_overlay(
                frame,
                shares,
                page,
                share_flow,
                account_form,
                state,
                window,
                cx,
            ),
            Overlay::SharingSetup(target) => {
                Self::reconcile_setup_overlay(frame, target, shares, setup_form, state, window, cx)
            }
            _ => OverlayCompletion::Keep,
        }
    }

    #[allow(clippy::too_many_arguments)]
    fn reconcile_account_overlay(
        frame: &crate::overlays::OverlayFrame,
        shares: &[crate::model::Share],
        page: Page,
        share_flow: crate::flow_state::ShareFlow,
        account_form: &mut super::AccountForm,
        state: &OverlayReconcile<'_>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> OverlayCompletion {
        if state.previous_account_page != super::AccountPage::Overview
            && account_form.page() == super::AccountPage::Overview
        {
            if let Some(Ok(message)) = account_form.feedback {
                account_form.feedback = None;
                return OverlayCompletion::CloseWithToast(message);
            }
            let had_no_account = state
                .previous_model
                .account
                .as_ref()
                .is_some_and(|view| view.status.account.is_empty());
            let has_account = state
                .model
                .account
                .as_ref()
                .is_some_and(|view| !view.status.account.is_empty());
            if account_form.feedback.is_none() && had_no_account && has_account {
                return OverlayCompletion::Close;
            }
        }
        if page == Page::Share
            && !matches!(account_form.feedback, Some(Err(_)))
            && let crate::flow_state::ShareFlow::Running(run) = share_flow
            && let Some(op_id) = share_operation_id(shares, run.share_id)
            && host_share_state(state.previous_model, op_id)
                == Some(HostShareState::AwaitingAuthorization)
            && host_share_state(state.model, op_id).is_some_and(host_share_running)
        {
            return account_form
                .feedback
                .take()
                .and_then(Result::ok)
                .map_or(OverlayCompletion::Close, OverlayCompletion::CloseWithToast);
        }
        let next_field = account_form.field_focus(state.model, cx);
        if state.previous_field != next_field.as_ref() {
            window.focus(next_field.as_ref().unwrap_or(&frame.scope), cx);
        }
        OverlayCompletion::Keep
    }

    fn reconcile_setup_overlay(
        frame: &crate::overlays::OverlayFrame,
        target: SharingSetupTarget,
        shares: &[crate::model::Share],
        setup_form: &mut super::SetupForm,
        state: &OverlayReconcile<'_>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> OverlayCompletion {
        let finished = state.had_setup_pending
            && setup_form.pending.is_none()
            && matches!(state.model.setup, windshare_host::Setup::Finished { .. });
        let should_close = match target {
            SharingSetupTarget::Defaults => finished,
            SharingSetupTarget::RecoverShare(id) | SharingSetupTarget::SwitchRelay(id) => {
                let op_id = share_operation_id(shares, id);
                let prev = op_id.and_then(|op| host_share_state(state.previous_model, op));
                let next = op_id.and_then(|op| host_share_state(state.model, op));
                let auth_completed = matches!(target, SharingSetupTarget::RecoverShare(_))
                    && prev == Some(HostShareState::AwaitingAuthorization)
                    && finished
                    && matches!(
                        &state.model.setup,
                        windshare_host::Setup::Finished { context }
                            if context.relay_operation == Some(RelayOperation::AuthorizeDevice)
                                && context.relay_saved
                    );
                let share_unblocked = prev.is_some_and(|s| !host_share_running(s))
                    && next.is_some_and(host_share_running);
                auth_completed || share_unblocked
            }
        };
        if should_close {
            return setup_form
                .feedback
                .take()
                .and_then(Result::ok)
                .map_or(OverlayCompletion::Close, OverlayCompletion::CloseWithToast);
        }
        if setup_form.page != state.previous_page {
            window.focus(&frame.scope, cx);
        }
        OverlayCompletion::Keep
    }

    pub fn stop_native_share(&self, id: usize) {
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        if matches!(session.quit_state, QuitState::Exiting) {
            return;
        }
        let Some(share) = self.shares.iter().find(|share| share.id == id) else {
            return;
        };
        let ShareSource::Native { operation_id } = &share.source else {
            return;
        };
        let accepted = session
            .model
            .operations
            .iter()
            .find(|op| &op.id == operation_id)
            .and_then(windshare_host::Operation::share);
        match accepted {
            Some(task) if task.state == windshare_host::ShareState::Sharing => {
                session.controller.stop(&task.task_id);
            }
            Some(task)
                if matches!(
                    task.state,
                    windshare_host::ShareState::Stopping | windshare_host::ShareState::Finished
                ) => {}
            _ => session.controller.cancel(operation_id),
        }
    }
}
