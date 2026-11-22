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
use windshare_host::{Model, RelayOperation, RelayPreferences, Setup, SetupRequest};

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
