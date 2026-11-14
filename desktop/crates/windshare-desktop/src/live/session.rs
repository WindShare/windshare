use crate::{
    desktop::Desktop,
    flow_state::{ShareFlow, ShareRun},
    localization::Text,
    model::{Page, ShareSource},
};
use gpui::{Context, Window};
use std::time::{Duration, SystemTime};
use windshare_host::{Connection, Controller, Model};

pub enum Runtime {
    Preview { delay: Duration },
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
    pub model: Model,
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
        matches!(self, Self::Preview { .. })
    }

    pub const fn selection_disabled(&self) -> bool {
        match self {
            Self::Preview { .. } => false,
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

    pub const fn status(&self) -> Text {
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
    pub const fn page_available(&self, page: Page) -> bool {
        self.runtime.is_preview() || matches!(page, Page::Share | Page::Shares | Page::Settings)
    }

    pub fn observe_runtime(controller: Controller, window: &Window, cx: &Context<Self>) {
        let weak = cx.entity().downgrade();
        window.on_window_should_close(cx, move |window, cx| {
            let _ = weak.update(cx, |this, cx| this.request_quit(window, cx));
            false
        });
        cx.spawn(async move |view, cx| {
            loop {
                controller.changed().await;
                if view.update(cx, Self::refresh_native).is_err() {
                    break;
                }
            }
        })
        .detach();
    }

    fn refresh_native(&mut self, cx: &mut Context<Self>) {
        let Runtime::Native(session) = &mut self.runtime else {
            return;
        };
        let model = session.controller.model();
        if model != session.model {
            for share in &mut self.shares {
                let ShareSource::Native { operation_id } = &share.source else {
                    continue;
                };
                if let Some(operation) = model.operations.iter().find(|op| &op.id == operation_id) {
                    super::projection::update_share(share, operation);
                }
            }
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
                .filter(|at| *at > SystemTime::now())
                .collect();
            deadlines.sort_unstable();
            deadlines.dedup();
            session.retry_wakeup = Some(cx.spawn(async move |view, cx| {
                for at in deadlines {
                    let delay = at.duration_since(SystemTime::now()).unwrap_or_default();
                    cx.background_executor().timer(delay).await;
                    if view.update(cx, |_, cx| cx.notify()).is_err() {
                        break;
                    }
                }
            }));
            session.model = model;
            for share in &self.shares {
                if let ShareSource::Native { operation_id } = &share.source
                    && let Some(run) = session.presentations.get_mut(operation_id)
                {
                    super::presentation::update_readiness(run, share);
                    if matches!(self.share_flow, ShareFlow::Running(current) if current.selection_id == share.id)
                    {
                        self.share_flow = ShareFlow::Running(*run);
                    }
                }
            }
            cx.notify();
        }
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
