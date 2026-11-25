use gpui::{Context, Window};
use std::sync::Arc;
use windshare_host::{Connection, Controller, Model, ShareState as HostShareState};

pub struct HostStore {
    controller: Controller,
    model: Arc<Model>,
    previous_model: Arc<Model>,
    exiting: bool,
}

impl HostStore {
    pub fn new(controller: Controller, window: &Window, cx: &Context<Self>) -> Self {
        let model = controller.model();
        let previous_model = Arc::clone(&model);
        let observer = controller.clone();
        // Controller::changed() uses a single-consumer MPMC channel; keep exactly
        // one task consuming it and fan out updates to views through GPUI observe.
        cx.spawn_in(window, async move |store, cx| {
            loop {
                observer.changed().await;
                if store.update(cx, Self::refresh).is_err() {
                    break;
                }
            }
        })
        .detach();
        Self {
            controller,
            model,
            previous_model,
            exiting: false,
        }
    }

    fn refresh(&mut self, cx: &mut Context<Self>) {
        let next = self.controller.model();
        if Arc::ptr_eq(&next, &self.model) {
            return;
        }
        self.previous_model = std::mem::replace(&mut self.model, next);
        cx.notify();
    }

    pub const fn model(&self) -> &Arc<Model> {
        &self.model
    }

    pub const fn previous_model(&self) -> &Arc<Model> {
        &self.previous_model
    }

    pub const fn controller(&self) -> &Controller {
        &self.controller
    }

    pub fn set_exiting(&mut self, exiting: bool, cx: &mut Context<Self>) {
        if self.exiting != exiting {
            self.exiting = exiting;
            cx.notify();
        }
    }

    pub fn selection_disabled(&self) -> bool {
        self.exiting
            || matches!(
                self.model.connection,
                Connection::Closing | Connection::Closed(_)
            )
    }

    pub fn submit_account(
        &mut self,
        command: windshare_host::AccountCommand,
        cx: &mut Context<Self>,
    ) -> Option<String> {
        let _ = self.controller.account(command);
        self.refresh(cx);
        self.model.account_request.id().map(str::to_owned)
    }

    pub fn submit_relay_setup(
        &mut self,
        operation_id: &str,
        relay_operation: windshare_host::RelayOperation,
        relay_url: String,
        access_key: String,
        cx: &mut Context<Self>,
    ) -> String {
        let id = self
            .controller
            .setup_relay(operation_id, relay_operation, relay_url, access_key);
        self.refresh(cx);
        id
    }

    pub fn submit_preference(
        &mut self,
        operation_id: &str,
        edit: windshare_host::PreferenceEdit,
        cx: &mut Context<Self>,
    ) -> String {
        let id = self.controller.edit_preference(operation_id, edit);
        self.refresh(cx);
        id
    }

    pub fn cancel_setup(&mut self, setup_id: &str, cx: &mut Context<Self>) -> bool {
        let accepted = self.controller.cancel_setup(setup_id);
        self.refresh(cx);
        accepted
    }
}

pub fn host_share_state<'a>(model: &'a Model, operation_id: &str) -> Option<&'a HostShareState> {
    model
        .operations
        .iter()
        .find(|op| op.id == operation_id)
        .and_then(windshare_host::Operation::share)
        .map(|share| &share.state)
}

pub const fn host_share_running(state: &HostShareState) -> bool {
    matches!(
        state,
        HostShareState::Preparing { .. } | HostShareState::Sharing { .. }
    )
}
