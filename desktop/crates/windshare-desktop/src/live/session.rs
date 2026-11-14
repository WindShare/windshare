use crate::{
    desktop::Desktop,
    flow_state::{ShareFlow, ShareReadiness},
    localization::Text,
    model::{Page, ShareSource},
};
use gpui::{Context, Window};
use std::time::{Duration, Instant, SystemTime};
use windshare_host::{Connection, Controller, Model};

pub enum Runtime {
    Preview { delay: Duration },
    Native(Box<Session>),
}

pub struct Session {
    pub controller: Controller,
    pub model: Model,
    pub picking: bool,
    pub quitting: bool,
    pub error: Option<String>,
    retry_wakeup: Option<gpui::Task<()>>,
}

impl Runtime {
    pub fn native(controller: Controller) -> Self {
        Self::Native(Box::new(Session {
            model: controller.model(),
            controller,
            picking: false,
            quitting: false,
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
                session.picking
                    || session.quitting
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

    pub const fn status(&self) -> Text {
        let Self::Native(session) = self else {
            return Text::PreviewStatus;
        };
        if session.quitting {
            return Text::ClosingShares;
        }
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
        window.on_window_should_close(cx, move |_, cx| {
            let _ = weak.update(cx, Self::quit);
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

    pub fn quit(&mut self, cx: &mut Context<Self>) {
        if let Runtime::Native(session) = &mut self.runtime {
            session.quitting = true;
            session.controller.begin_shutdown();
            cx.notify();
        } else {
            cx.quit();
        }
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
            // Readiness belongs to the task. The view clock may still be in flight,
            // and navigating elsewhere must not discard a result or create a task.
            if let ShareFlow::Running(run) = &mut self.share_flow
                && let Some(share) = self
                    .shares
                    .iter()
                    .find(|share| share.id == run.selection_id)
            {
                if share.finished() {
                    run.readiness = ShareReadiness::Finished;
                } else if share.usable_link().is_some()
                    && matches!(run.readiness, ShareReadiness::Pending)
                {
                    let at = Instant::now();
                    run.readiness = ShareReadiness::Ready { at };
                    tracing::info!(event = "share.ready", operation = run.operation,
                        source = ?share.source, elapsed_ms = at.duration_since(run.started_at).as_millis(),
                        preview = false);
                }
            }
            cx.notify();
        }
        if session.quitting && matches!(session.model.connection, Connection::Closed(_)) {
            cx.quit();
        }
    }

    pub fn stop_native_share(&self, id: usize) {
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        if session.quitting {
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
