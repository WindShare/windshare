use super::host_store::{HostStore, host_share_state};
use crate::{
    desktop::Desktop,
    flow_state::ShareRun,
    localization::Text,
    model::{Overlay, Page, ShareSource},
};
use gpui::{App, AppContext, Context, Entity, Subscription, Window};
use std::{
    collections::HashMap,
    sync::Arc,
    time::{Duration, SystemTime},
};
use windshare_host::{
    AccountProfile, AccountState, AccountStatus, AccountView, Allowance, Connection, Controller,
    Model, Nickname, RelayMode, RelayPreferences, SavedRelay, ShareState as HostShareState,
};

pub const PREVIEW_DEFAULT_FRONTEND: &str = "https://windshare.top";
const PREVIEW_REMAINING_BYTES: u64 = 1_932_735_283;

pub struct PreviewRuntime {
    pub delay: Duration,
    pub preferences: RelayPreferences,
    pub account: AccountView,
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
    pub host: Entity<HostStore>,
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

impl Session {
    pub fn model<'a>(&self, cx: &'a App) -> &'a Arc<Model> {
        self.host.read(cx).model()
    }

    pub fn controller<'a>(&self, cx: &'a App) -> &'a Controller {
        self.host.read(cx).controller()
    }
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

    pub fn native(
        controller: Controller,
        application: super::application::Application,
        window: &Window,
        cx: &mut App,
    ) -> Self {
        let host = cx.new(|cx| HostStore::new(controller, window, cx));
        Self::Native(Box::new(Session {
            host,
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

    pub fn selection_disabled(&self, cx: &App) -> bool {
        match self {
            Self::Preview(_) => false,
            Self::Native(session) => session.host.read(cx).selection_disabled(),
        }
    }

    pub fn notice(&self, cx: &App) -> Option<String> {
        let Self::Native(session) = self else {
            return None;
        };
        let model = session.model(cx);
        session
            .error
            .clone()
            .or_else(|| model.command_error.clone())
            .or_else(|| {
                if let Connection::Closed(Err(error)) = &model.connection {
                    Some(error.clone())
                } else {
                    None
                }
            })
    }

    pub fn setup_cancellation_error<'a>(&self, cx: &'a App) -> Option<&'a str> {
        if let Self::Native(session) = self
            && let windshare_host::Setup::CancelFailed { error, .. } = &session.model(cx).setup
        {
            Some(&error.message)
        } else {
            None
        }
    }

    pub fn status(&self, cx: &App) -> Text {
        let Self::Native(session) = self else {
            return Text::PreviewStatus;
        };
        match session.model(cx).connection {
            Connection::Starting => Text::StartingHost,
            Connection::Connected => Text::ReadyToShare,
            Connection::Reconnecting(_) => Text::ControlsReconnecting,
            Connection::Closing => Text::ClosingShares,
            Connection::Closed(_) => Text::HostClosed,
        }
    }
}

impl Desktop {
    pub(crate) const fn interaction_epoch(&self) -> u64 {
        match &self.runtime {
            Runtime::Native(session) => session.interaction_epoch,
            Runtime::Preview(_) => 0,
        }
    }

    pub const fn page_available(&self, page: Page) -> bool {
        self.runtime.is_preview() || matches!(page, Page::Share | Page::Shares | Page::Settings)
    }

    pub fn observe_runtime(
        host: &Entity<HostStore>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Subscription {
        let weak = cx.entity().downgrade();
        window.on_window_should_close(cx, move |window, cx| {
            let _ = weak.update(cx, |this, cx| this.request_quit(window, cx));
            false
        });
        cx.observe_in(host, window, |this, host, window, cx| {
            this.on_host_changed(&host, window, cx);
        })
    }

    fn on_host_changed(
        &mut self,
        host: &Entity<HostStore>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let (previous_model, model) = {
            let store = host.read(cx);
            (
                Arc::clone(store.previous_model()),
                Arc::clone(store.model()),
            )
        };
        if let Runtime::Native(session) = &mut self.runtime {
            session.retry_wakeup = Self::schedule_retry_wakeups(&model, cx);
        }
        if let windshare_host::Setup::Running {
            verification_url,
            service_login: None,
            ..
        } = &model.setup
            && !verification_url.is_empty()
            && !matches!(
                &previous_model.setup,
                windshare_host::Setup::Running { verification_url: previous, .. } if previous == verification_url
            )
        {
            cx.open_url(verification_url);
        }
        self.network_ui.prune_expanded(&model.preferences);
        self.project_native_shares(&model);
        if let Some(toast) = self.network_ui.take_remove_toast(&model) {
            self.notify(toast, cx);
        }
        if self.should_close_unblocked_account_overlay(&previous_model, &model, cx) {
            self.close_overlay(window, cx);
        }
        cx.notify();
    }

    fn should_close_unblocked_account_overlay(
        &self,
        previous_model: &Model,
        model: &Model,
        cx: &App,
    ) -> bool {
        let Some(Overlay::Account(dialog)) = self.overlays.last().map(|frame| &frame.content)
        else {
            return false;
        };
        let crate::flow_state::ShareFlow::Running(run) = self.share_flow else {
            return false;
        };
        let Some(op_id) = share_operation_id(&self.shares, run.share_id) else {
            return false;
        };
        let dialog = dialog.read(cx);
        if dialog.has_error_feedback()
            || dialog.will_complete_on(model)
            || matches!(
                model.account_request,
                windshare_host::AccountRequest::Failed { .. }
            )
        {
            return false;
        }
        self.page == Page::Share
            && matches!(
                host_share_state(previous_model, op_id),
                Some(HostShareState::AwaitingAuthorization { .. })
            )
            && host_share_state(model, op_id).is_some_and(|state| {
                matches!(
                    state,
                    HostShareState::Admitting
                        | HostShareState::Preparing { .. }
                        | HostShareState::Sharing { .. }
                )
            })
    }

    fn project_native_shares(&mut self, model: &Model) {
        if self.shares.is_empty() || model.operations.is_empty() {
            return;
        }
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
                    tracing::info!(
                        event = "share.ready",
                        operation = %operation.id,
                        share_id = share.id,
                        preview = false
                    );
                }
            }
        }
    }

    fn schedule_retry_wakeups(model: &Model, cx: &Context<Self>) -> Option<gpui::Task<()>> {
        // This timer only re-enables presentation at the host's deadline.
        // It never refreshes the directory or retries an operation.
        let mut deadlines: Vec<_> = model
            .operations
            .iter()
            .filter_map(|op| op.share())
            .filter_map(|share| match share.state {
                windshare_host::ShareState::AwaitingNetwork { blocking, .. } => {
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

    pub fn stop_native_share(&self, id: usize, cx: &App) {
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
        let store = session.host.read(cx);
        let accepted = store
            .model()
            .operations
            .iter()
            .find(|op| &op.id == operation_id)
            .and_then(windshare_host::Operation::share);
        match accepted.map(|task| &task.state) {
            Some(windshare_host::ShareState::Sharing { task_id, .. }) => {
                store.controller().stop(task_id);
            }
            Some(
                windshare_host::ShareState::Stopping { .. }
                | windshare_host::ShareState::Finished { .. },
            ) => {}
            _ => store.controller().cancel(operation_id),
        }
    }
}
