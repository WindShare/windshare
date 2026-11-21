use super::{SetupPage, SetupReconcile};
use crate::{
    localization::{LanguageSettings, Text, language},
    model::SharingSetupTarget,
};
use gpui::{
    AppContext, Context, Entity, EventEmitter, FocusHandle, Focusable, Subscription, Window,
};
use windshare_host::{PreferenceEdit, RelayOperation, RelayPreferences, Setup};
use windshare_ui::{Icon, Input, InputEvent};

#[derive(Clone)]
pub struct SetupShareSummary {
    pub id: usize,
    pub name: String,
    pub symbol: Icon,
    pub finished: bool,
    pub live: bool,
    pub operation_id: String,
}

#[derive(Clone)]
pub struct OtherRunningSetup {
    pub share_name: Option<String>,
}

#[derive(Clone, Copy)]
pub enum SetupRuntimeMode {
    Preview,
    Native {
        waits_official: bool,
        selection_disabled: bool,
        pending: bool,
    },
}

impl SetupRuntimeMode {
    pub const fn is_preview(self) -> bool {
        matches!(self, Self::Preview)
    }

    pub const fn pending(self) -> bool {
        matches!(self, Self::Native { pending: true, .. })
    }

    pub const fn selection_disabled(self) -> bool {
        matches!(
            self,
            Self::Native {
                selection_disabled: true,
                ..
            }
        )
    }

    pub const fn waits_official(self) -> bool {
        matches!(
            self,
            Self::Native {
                waits_official: true,
                ..
            }
        )
    }
}

#[derive(Clone)]
pub struct SetupDialogSnapshot {
    pub target: SharingSetupTarget,
    pub prefs: RelayPreferences,
    pub setup: Setup,
    pub command_error: Option<String>,
    pub mode: SetupRuntimeMode,
    pub share: Option<SetupShareSummary>,
    pub other_running: Option<OtherRunningSetup>,
    pub feedback: Option<Result<Text, String>>,
    pub expanded_relay: Option<String>,
}

#[derive(Clone)]
pub enum SetupDialogEvent {
    PageChanged,
    SubmitRelay {
        share_id: Option<usize>,
        relay_operation: RelayOperation,
        relay_url: String,
        access_key: String,
    },
    SubmitFrontend {
        share_id: Option<usize>,
        url: String,
    },
    EditPreference(PreferenceEdit),
    ToggleRelayActions(String),
    SwitchShareRelay {
        share_id: usize,
        relay_url: String,
    },
    UseSavedSettings(usize),
    ReturnToRunningSetup,
    CancelSetup,
    ApproveRelay,
    ConfirmReauthorizeRelay(String),
    ConfirmRemoveRelay(String),
    Close,
}

pub struct SetupDialog {
    pub(super) page: SetupPage,
    pub(super) relay: Entity<Input>,
    pub(super) access_key: Entity<Input>,
    pub(super) frontend: Entity<Input>,
    pub(super) snapshot: SetupDialogSnapshot,
    overlay_scope: FocusHandle,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<SetupDialogEvent> for SetupDialog {}

impl SetupDialog {
    pub fn new(
        snapshot: SetupDialogSnapshot,
        initial_page: SetupPage,
        initial_relay_url: Option<String>,
        overlay_scope: FocusHandle,
        window: &Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let locale = language(cx);
        let relay = cx.new(|cx| {
            Input::new(locale.text(Text::RelayAddress), cx).placeholder("https://relay.example")
        });
        let frontend = cx.new(|cx| {
            Input::new(locale.text(Text::ReceiverAddress), cx).placeholder("https://share.example")
        });
        let access_key = cx.new(|cx| Input::new(locale.text(Text::ServiceKey), cx).password());
        let mut subscriptions =
            vec![
                cx.observe_global_in::<LanguageSettings>(window, |this, _, cx| {
                    this.localize(cx);
                    cx.notify();
                }),
            ];
        for input in [&relay, &frontend, &access_key] {
            subscriptions.push(cx.subscribe_in(
                input,
                window,
                |this, _, event, _, cx| match event {
                    InputEvent::Submit => this.submit(cx),
                    InputEvent::Changed => cx.notify(),
                },
            ));
        }
        let mut dialog = Self {
            page: SetupPage::Selection,
            relay,
            access_key,
            frontend,
            snapshot,
            overlay_scope,
            _subscriptions: subscriptions,
        };
        // Defer window focus to Desktop::open_scoped_overlay so it captures the
        // pre-dialog return focus before moving focus into this modal.
        dialog.configure_page(initial_page, initial_relay_url, cx);
        dialog
    }

    fn localize(&self, cx: &mut Context<Self>) {
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

    pub const fn title(&self) -> Text {
        self.page.title()
    }

    pub(super) fn operation_id(&self) -> &str {
        self.snapshot
            .share
            .as_ref()
            .map_or("", |share| share.operation_id.as_str())
    }

    pub(super) fn input_submittable(&self, cx: &Context<Self>) -> bool {
        match self.page {
            SetupPage::Selection => false,
            SetupPage::Relay(_) => !self.relay.read(cx).value().trim().is_empty(),
            SetupPage::ReceiverSite => {
                let url = self.frontend.read(cx).value().trim();
                !url.is_empty() && url != self.snapshot.prefs.frontend
            }
        }
    }

    pub(super) fn shows_setup_progress(&self) -> bool {
        let connecting = self
            .snapshot
            .setup
            .context()
            .is_some_and(|ctx| !ctx.relay_url.is_empty());
        self.snapshot.setup.busy() && connecting || self.snapshot.setup.requires_cancellation()
    }

    fn form_visible(&self) -> bool {
        self.snapshot.other_running.is_none() && !self.shows_setup_progress()
    }

    pub fn focus_target(&self, cx: &gpui::App) -> FocusHandle {
        if !self.form_visible() {
            return self.overlay_scope.clone();
        }
        match self.page {
            SetupPage::Selection => self.overlay_scope.clone(),
            SetupPage::Relay(_) => self.relay.read(cx).focus_handle(cx),
            SetupPage::ReceiverSite => self.frontend.read(cx).focus_handle(cx),
        }
    }

    pub(super) fn set_page(
        &mut self,
        page: SetupPage,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if self.snapshot.mode.pending() || self.snapshot.setup.unfinished() {
            return;
        }
        self.switch_page(page, None, window, cx);
    }

    pub fn prepare_relay_authorization(
        &mut self,
        relay_url: String,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        if self.snapshot.mode.pending() || self.snapshot.setup.unfinished() {
            return;
        }
        self.switch_page(
            SetupPage::Relay(RelayOperation::AuthorizeDevice),
            Some(relay_url),
            window,
            cx,
        );
        self.submit(cx);
    }

    fn switch_page(
        &mut self,
        page: SetupPage,
        prefill_relay: Option<String>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        self.configure_page(page, prefill_relay, cx);
        let focus = self.focus_target(cx);
        window.focus(&focus, cx);
        cx.emit(SetupDialogEvent::PageChanged);
        cx.notify();
    }

    fn configure_page(
        &mut self,
        page: SetupPage,
        prefill_relay: Option<String>,
        cx: &mut Context<Self>,
    ) {
        self.page = page;
        self.snapshot.feedback = None;
        self.access_key
            .update(cx, |input, cx| input.set_value("", cx));
        match page {
            SetupPage::Selection => {}
            SetupPage::Relay(_) => {
                let relay_value = prefill_relay.unwrap_or_default();
                self.relay
                    .update(cx, |input, cx| input.set_value(relay_value, cx));
            }
            SetupPage::ReceiverSite => {
                let frontend = self.snapshot.prefs.frontend.clone();
                self.frontend
                    .update(cx, |input, cx| input.set_value(frontend, cx));
            }
        }
    }

    pub(super) fn submit(&self, cx: &mut Context<Self>) {
        let operation = self.operation_id();
        if !self.snapshot.mode.is_preview()
            && (self.snapshot.mode.selection_disabled()
                || self.snapshot.mode.pending()
                || !self.snapshot.setup.can_configure(operation))
        {
            return;
        }
        let share_id = self.snapshot.target.share_id();
        match self.page {
            SetupPage::Selection => {}
            SetupPage::Relay(relay_operation) => {
                let relay_url = self.relay.read(cx).value().trim().to_owned();
                if relay_url.is_empty() {
                    return;
                }
                let access_key = self.access_key.read(cx).value().trim().to_owned();
                cx.emit(SetupDialogEvent::SubmitRelay {
                    share_id,
                    relay_operation,
                    relay_url,
                    access_key,
                });
            }
            SetupPage::ReceiverSite => {
                let url = self.frontend.read(cx).value().trim().to_owned();
                if url.is_empty() || url == self.snapshot.prefs.frontend {
                    return;
                }
                cx.emit(SetupDialogEvent::SubmitFrontend { share_id, url });
            }
        }
    }

    pub fn sync_snapshot(
        &mut self,
        snapshot: SetupDialogSnapshot,
        update: Option<&SetupReconcile>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let previous_page = self.page;
        let previous_form_visible = self.form_visible();
        if let Some(update) = update {
            if update.clear_access_key {
                self.access_key
                    .update(cx, |input, cx| input.set_value("", cx));
            }
            if update.finished {
                self.page = SetupPage::Selection;
            }
        }
        self.snapshot = snapshot;
        if self.page != previous_page || self.form_visible() != previous_form_visible {
            let focus = self.focus_target(cx);
            window.focus(&focus, cx);
        }
        cx.notify();
    }

    pub fn clear_access_key(&self, cx: &mut Context<Self>) {
        self.access_key
            .update(cx, |input, cx| input.set_value("", cx));
    }
}
