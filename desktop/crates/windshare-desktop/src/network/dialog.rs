use super::{
    SetupPage,
    dialog_host::{
        OtherRunningSetup, ShareDisplay, operation_display, operation_finished,
        other_running_for_model, setup_shows_progress,
    },
};
use crate::{
    live::HostStore,
    localization::{LanguageSettings, Text, language},
    model::SharingSetupTarget,
};
use gpui::{
    App, AppContext, Context, Entity, EventEmitter, FocusHandle, Focusable, Subscription, Window,
};
use windshare_host::{PreferenceEdit, RelayOperation, RelayPreferences, Setup};
use windshare_ui::{Input, InputEvent};

const IDLE_SETUP: Setup = Setup::Idle;

pub enum SetupSource {
    Native {
        host: Entity<HostStore>,
        operation_id: String,
    },
    Preview {
        prefs: RelayPreferences,
    },
}

#[derive(Clone)]
pub enum SetupDialogEvent {
    PageChanged,
    FocusChanged,
    ReturnToRunningSetup,
    Completed(Option<Text>),
    Close,
    PreviewRelay(String),
    PreviewFrontend(String),
    PreviewPreference(PreferenceEdit),
}

pub struct SetupDialog {
    pub(super) target: SharingSetupTarget,
    pub(super) source: SetupSource,
    pub(super) page: SetupPage,
    pub(super) relay: Entity<Input>,
    pub(super) access_key: Entity<Input>,
    pub(super) frontend: Entity<Input>,
    pub(super) expanded_relay: Option<String>,
    pub(super) feedback: Option<Result<Text, String>>,
    pub(super) in_flight_id: Option<String>,
    overlay_scope: FocusHandle,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<SetupDialogEvent> for SetupDialog {}

impl SetupDialog {
    pub fn new(
        target: SharingSetupTarget,
        source: SetupSource,
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
        let in_flight_id = match &source {
            SetupSource::Native { host, operation_id } => {
                subscriptions.push(cx.observe(host, |this, host, cx| {
                    this.on_host_updated(&host, cx);
                }));
                host.read(cx)
                    .model()
                    .in_flight_setup_id(operation_id)
                    .map(str::to_owned)
            }
            SetupSource::Preview { .. } => None,
        };
        let mut dialog = Self {
            target,
            source,
            page: SetupPage::Selection,
            relay,
            access_key,
            frontend,
            expanded_relay: None,
            feedback: None,
            in_flight_id,
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

    pub(super) const fn is_preview(&self) -> bool {
        matches!(self.source, SetupSource::Preview { .. })
    }

    pub(super) const fn operation_id(&self) -> &str {
        match &self.source {
            SetupSource::Native { operation_id, .. } => operation_id.as_str(),
            SetupSource::Preview { .. } => "",
        }
    }

    pub(super) fn prefs<'a>(&'a self, cx: &'a App) -> &'a RelayPreferences {
        match &self.source {
            SetupSource::Native { host, .. } => &host.read(cx).model().preferences,
            SetupSource::Preview { prefs } => prefs,
        }
    }

    pub(super) fn setup<'a>(&'a self, cx: &'a App) -> &'a Setup {
        match &self.source {
            SetupSource::Native { host, .. } => &host.read(cx).model().setup,
            SetupSource::Preview { .. } => &IDLE_SETUP,
        }
    }

    pub(super) fn command_error<'a>(&'a self, cx: &'a App) -> Option<&'a str> {
        match &self.source {
            SetupSource::Native { host, .. } => host.read(cx).model().command_error.as_deref(),
            SetupSource::Preview { .. } => None,
        }
    }

    pub(super) fn pending(&self, cx: &App) -> bool {
        match &self.source {
            SetupSource::Native { host, .. } => host.read(cx).model().setup_pending(),
            SetupSource::Preview { .. } => false,
        }
    }

    pub(super) fn selection_disabled(&self, cx: &App) -> bool {
        match &self.source {
            SetupSource::Native { host, .. } => host.read(cx).selection_disabled(),
            SetupSource::Preview { .. } => false,
        }
    }

    pub(super) fn share_finished(&self, cx: &App) -> bool {
        match &self.source {
            SetupSource::Native { host, operation_id } => {
                operation_finished(host.read(cx).model(), operation_id)
            }
            SetupSource::Preview { .. } => false,
        }
    }

    pub(super) fn busy(&self, cx: &App) -> bool {
        self.pending(cx)
            || self.selection_disabled(cx)
            || self.setup(cx).unfinished()
            || self.share_finished(cx)
    }

    pub(super) fn cancel_disabled(&self, cx: &App) -> bool {
        match &self.source {
            SetupSource::Native { host, .. } => {
                let store = host.read(cx);
                store.selection_disabled() || !store.model().can_cancel_current_setup()
            }
            SetupSource::Preview { .. } => true,
        }
    }

    pub(super) fn waits_official(&self, cx: &App) -> bool {
        match &self.source {
            SetupSource::Native { host, operation_id } => host
                .read(cx)
                .model()
                .waits_for_official_authorization(operation_id),
            SetupSource::Preview { .. } => false,
        }
    }

    pub(super) fn share_summary(&self, cx: &App) -> Option<ShareDisplay> {
        let SetupSource::Native { host, operation_id } = &self.source else {
            return None;
        };
        if operation_id.is_empty() {
            return None;
        }
        let op = host
            .read(cx)
            .model()
            .operations
            .iter()
            .find(|op| &op.id == operation_id)?;
        Some(operation_display(op))
    }

    pub(super) fn other_running_setup(&self, cx: &App) -> Option<OtherRunningSetup> {
        let SetupSource::Native { host, operation_id } = &self.source else {
            return None;
        };
        other_running_for_model(host.read(cx).model(), operation_id)
    }

    pub(super) fn input_submittable(&self, cx: &Context<Self>) -> bool {
        match self.page {
            SetupPage::Selection => false,
            SetupPage::Relay(_) => !self.relay.read(cx).value().trim().is_empty(),
            SetupPage::ReceiverSite => {
                let url = self.frontend.read(cx).value().trim();
                !url.is_empty() && url != self.prefs(cx).frontend
            }
        }
    }

    pub(super) fn shows_setup_progress(&self, cx: &App) -> bool {
        setup_shows_progress(self.setup(cx))
    }

    fn form_visible(&self, cx: &App) -> bool {
        self.other_running_setup(cx).is_none() && !self.shows_setup_progress(cx)
    }

    pub fn focus_target(&self, cx: &App) -> FocusHandle {
        if !self.form_visible(cx) {
            return self.overlay_scope.clone();
        }
        match self.page {
            SetupPage::Selection => self.overlay_scope.clone(),
            SetupPage::Relay(_) => self.relay.read(cx).focus_handle(cx),
            SetupPage::ReceiverSite => self.frontend.read(cx).focus_handle(cx),
        }
    }

    pub(super) fn set_page(&mut self, page: SetupPage, cx: &mut Context<Self>) {
        if self.pending(cx) || self.setup(cx).unfinished() {
            return;
        }
        self.switch_page(page, None, cx);
    }

    pub fn prepare_relay_authorization(&mut self, relay_url: String, cx: &mut Context<Self>) {
        if self.pending(cx) || self.setup(cx).unfinished() {
            return;
        }
        self.switch_page(
            SetupPage::Relay(RelayOperation::AuthorizeDevice),
            Some(relay_url),
            cx,
        );
        self.submit(cx);
    }

    fn switch_page(
        &mut self,
        page: SetupPage,
        prefill_relay: Option<String>,
        cx: &mut Context<Self>,
    ) {
        self.configure_page(page, prefill_relay, cx);
        // Desktop owns overlay focus so only the active top dialog can move window focus.
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
        self.feedback = None;
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
                let frontend = self.prefs(cx).frontend.clone();
                self.frontend
                    .update(cx, |input, cx| input.set_value(frontend, cx));
            }
        }
    }

    pub(super) fn toggle_relay_actions(&mut self, relay_url: String, cx: &mut Context<Self>) {
        self.expanded_relay = if self.expanded_relay.as_deref() == Some(relay_url.as_str()) {
            None
        } else {
            Some(relay_url)
        };
        cx.notify();
    }

    pub fn set_preview_preferences(&mut self, prefs: RelayPreferences, cx: &mut Context<Self>) {
        if let SetupSource::Preview {
            prefs: current_prefs,
        } = &mut self.source
        {
            if let Some(url) = &self.expanded_relay
                && !prefs.relays.iter().any(|relay| &relay.url == url)
            {
                self.expanded_relay = None;
            }
            *current_prefs = prefs;
            cx.notify();
        }
    }

    pub fn clear_access_key(&self, cx: &mut Context<Self>) {
        self.access_key
            .update(cx, |input, cx| input.set_value("", cx));
    }
}
