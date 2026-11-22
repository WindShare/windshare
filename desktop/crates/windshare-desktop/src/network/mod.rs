mod dialog;
mod dialog_host;
mod dialog_view;
mod relay_preferences;
mod settings;
mod setup;
mod setup_feedback;

pub use dialog::{SetupDialog, SetupDialogEvent, SetupSource};
pub use settings::setting_row;

use crate::localization::Text;
use windshare_host::{Model, RelayOperation, RelayPreferences, Setup, SetupContext, SetupRequest};

#[cfg(test)]
#[path = "network_tests.rs"]
mod tests;

const MAX_ACTIVE_RELAYS: usize = 8;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SetupPage {
    Selection,
    Relay(RelayOperation),
    ReceiverSite,
}

impl SetupPage {
    pub const fn title(self) -> Text {
        match self {
            Self::Selection => Text::ChooseRelays,
            Self::Relay(RelayOperation::AddProfile) => Text::ConnectRelay,
            Self::Relay(RelayOperation::AuthorizeDevice) => Text::ApproveDevice,
            Self::ReceiverSite => Text::ReceiverSite,
        }
    }
}

fn setup_finished_feedback(model: &Model) -> Option<Text> {
    let Setup::Finished { context } = &model.setup else {
        return None;
    };
    if context.frontend_saved {
        Some(Text::FrontendSaved)
    } else if context.relay_saved {
        let enabled = model
            .preferences
            .relays
            .iter()
            .any(|relay| relay.url == context.relay_url && relay.enabled);
        Some(
            if model.waits_for_official_authorization(&context.operation_id) {
                Text::RelaySaved
            } else if enabled {
                Text::RelayConnected
            } else {
                Text::RelaySavedInactive
            },
        )
    } else {
        None
    }
}

fn settings_visible_setup(setup: &Setup) -> Option<&SetupContext> {
    setup.context().filter(|ctx| {
        setup.unfinished() && (ctx.operation_id.is_empty() || setup.requires_cancellation())
    })
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct SetupDialogHandoff {
    pub in_flight_id: Option<String>,
    pub remove_toast_id: Option<String>,
}

impl SetupDialogHandoff {
    pub const fn is_empty(&self) -> bool {
        self.in_flight_id.is_none() && self.remove_toast_id.is_none()
    }
}

#[derive(Default)]
pub struct NetworkSettingsState {
    pub expanded_relay: Option<String>,
    pub inline_request_id: Option<String>,
    pub remove_toast_id: Option<String>,
}

impl NetworkSettingsState {
    pub fn clear_transient(&mut self) {
        self.expanded_relay = None;
        self.inline_request_id = None;
    }

    pub fn adopt_dialog_handoff(&mut self, handoff: SetupDialogHandoff, settings_visible: bool) {
        if settings_visible && let Some(id) = handoff.in_flight_id {
            self.inline_request_id = Some(id);
        }
        if let Some(id) = handoff.remove_toast_id {
            self.remove_toast_id = Some(id);
        }
    }

    pub fn observe_active_setup(&mut self, model: &Model) {
        if let Some(context) = settings_visible_setup(&model.setup) {
            self.inline_request_id = Some(context.id.clone());
        }
    }

    pub fn prune_expanded(&mut self, prefs: &RelayPreferences) {
        if let Some(url) = &self.expanded_relay
            && !prefs.relays.iter().any(|relay| &relay.url == url)
        {
            self.expanded_relay = None;
        }
    }

    pub fn take_remove_toast(&mut self, model: &Model) -> Option<Text> {
        let tracked = self.remove_toast_id.as_deref()?;
        match &model.setup_request {
            SetupRequest::Finished { id } if id == tracked => {
                self.remove_toast_id = None;
                Some(Text::RelayRemoved)
            }
            SetupRequest::Cancelled { id } | SetupRequest::Failed { id, .. } if id == tracked => {
                self.remove_toast_id = None;
                None
            }
            _ => None,
        }
    }

    pub fn inline_feedback(&self, model: &Model) -> Option<Result<Text, String>> {
        let tracked = self.inline_request_id.as_deref()?;
        match &model.setup_request {
            SetupRequest::Finished { id } if id == tracked => {
                setup_finished_feedback(model).map(Ok)
            }
            SetupRequest::Cancelled { id } if id == tracked => Some(Ok(Text::SetupCancelled)),
            SetupRequest::Failed { id, error } if id == tracked && !model.setup.unfinished() => {
                Some(Err(error.message.clone()))
            }
            _ => None,
        }
    }
}
