mod dialog;
mod dialog_view;
mod relay_preferences;
mod settings;
mod setup;
mod setup_feedback;

pub use dialog::{
    OtherRunningSetup, SetupDialog, SetupDialogEvent, SetupDialogSnapshot, SetupRuntimeMode,
    SetupShareSummary,
};
pub use settings::setting_row;

use crate::localization::Text;
use windshare_host::{Model, RelayOperation, Setup};

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

#[derive(Default)]
pub struct SetupReconcile {
    pub toast: Option<Text>,
    pub had_pending: bool,
    pub finished: bool,
    pub clear_access_key: bool,
}

#[derive(Default)]
pub struct SetupTracker {
    pub pending: Option<String>,
    pub pending_toast: Option<Text>,
    pub feedback: Option<Result<Text, String>>,
    pub expanded_relay: Option<String>,
}

impl SetupTracker {
    pub fn reconcile(&mut self, model: &Model) -> SetupReconcile {
        if let Some(url) = &self.expanded_relay
            && !model
                .preferences
                .relays
                .iter()
                .any(|relay| &relay.url == url)
        {
            self.expanded_relay = None;
        }
        let Some(id) = self.pending.as_ref() else {
            return SetupReconcile::default();
        };
        let mut update = SetupReconcile {
            had_pending: true,
            ..SetupReconcile::default()
        };
        if let Some(error) = model.settings_rejection(id) {
            self.feedback = Some(Err(error.message.clone()));
            self.pending_toast = None;
        } else if model
            .setup
            .context()
            .is_some_and(|context| &context.id == id)
        {
            self.feedback = match &model.setup {
                Setup::Finished { context } => {
                    update.finished = true;
                    update.clear_access_key = true;
                    update.toast = self.pending_toast.take();
                    if context.frontend_saved {
                        Some(Ok(Text::FrontendSaved))
                    } else if context.relay_saved {
                        let enabled = model
                            .preferences
                            .relays
                            .iter()
                            .any(|relay| relay.url == context.relay_url && relay.enabled);
                        Some(Ok(
                            if model.waits_for_official_authorization(&context.operation_id) {
                                Text::RelaySaved
                            } else if enabled {
                                Text::RelayConnected
                            } else {
                                Text::RelaySavedInactive
                            },
                        ))
                    } else {
                        None
                    }
                }
                Setup::Cancelled { .. } => {
                    self.pending_toast = None;
                    update.clear_access_key = true;
                    Some(Ok(Text::SetupCancelled))
                }
                Setup::Failed { error, .. } if !model.setup.unfinished() => {
                    self.pending_toast = None;
                    Some(Err(error.message.clone()))
                }
                Setup::Failed { .. } | Setup::CancelFailed { .. } => {
                    self.pending_toast = None;
                    None
                }
                _ => return SetupReconcile::default(),
            };
        } else {
            return SetupReconcile::default();
        }
        self.pending = None;
        update
    }
}
