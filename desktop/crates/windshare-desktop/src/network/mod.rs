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
use windshare_host::{
    Model, RelayOperation, RelayPreferences, Setup, SetupContext, SetupRequest, SetupSettlement,
};

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

/// Banner presentation for `Settings -> Relays & network` while the host's
/// single global setup workflow is unfinished. Share-scoped setups that do not
/// require cancellation still lock all network controls, so settings surfaces a
/// return path instead of adopting their completion/error ownership.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum SettingsSetupBanner<'a> {
    InlineProgress,
    ReturnToShare(&'a SetupContext),
}

fn settings_setup_banner(setup: &Setup) -> Option<SettingsSetupBanner<'_>> {
    if !setup.unfinished() {
        return None;
    }
    if settings_visible_setup(setup).is_some() {
        return Some(SettingsSetupBanner::InlineProgress);
    }
    setup.context().map(SettingsSetupBanner::ReturnToShare)
}

pub fn relay_removed_toast(previous: &Model, current: &Model) -> Option<Text> {
    let Setup::Finished { context } = &current.setup else {
        return None;
    };
    if !context.relay_removed {
        return None;
    }
    let already_shown = matches!(
        &previous.setup,
        Setup::Finished { context: prev } if prev.id == context.id && prev.relay_removed
    );
    (!already_shown).then_some(Text::RelayRemoved)
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct CommunityNoticeState {
    seen: u64,
    visible: bool,
}

impl CommunityNoticeState {
    /// Tracks host notice counters starting from 0 so a notice already latched
    /// in the initial host model before `ModelCursor` attaches still surfaces.
    pub const fn observe(&mut self, notice: u64) -> bool {
        if notice > self.seen {
            self.seen = notice;
            self.visible = true;
            return true;
        }
        false
    }

    pub const fn visible(self) -> bool {
        self.visible
    }

    pub const fn dismiss(&mut self) -> bool {
        let was_visible = self.visible;
        self.visible = false;
        was_visible
    }
}

#[derive(Default)]
pub struct NetworkSettingsState {
    pub expanded_relay: Option<String>,
    pub inline_request_id: Option<String>,
}

impl NetworkSettingsState {
    pub fn clear_transient(&mut self) {
        self.expanded_relay = None;
        self.inline_request_id = None;
    }

    pub fn observe_active_setup(&mut self, model: &Model) -> Option<&str> {
        let active_id = match &model.setup_request {
            SetupRequest::Pending {
                id, operation_id, ..
            } if operation_id.is_empty() => Some(id.as_str()),
            _ => settings_visible_setup(&model.setup).map(|context| context.id.as_str()),
        }?;
        if self.inline_request_id.as_deref() == Some(active_id) {
            return None;
        }
        self.inline_request_id = Some(active_id.to_owned());
        self.inline_request_id.as_deref()
    }

    pub fn prune_expanded(&mut self, prefs: &RelayPreferences) {
        if let Some(url) = &self.expanded_relay
            && !prefs.relays.iter().any(|relay| &relay.url == url)
        {
            self.expanded_relay = None;
        }
    }

    pub fn inline_feedback(&self, model: &Model) -> Option<Result<Text, String>> {
        let tracked = self.inline_request_id.as_deref()?;
        match model.setup_settlement(tracked)? {
            SetupSettlement::Finished => setup_finished_feedback(model).map(Ok),
            SetupSettlement::Cancelled => Some(Ok(Text::SetupCancelled)),
            SetupSettlement::Failed(error) => {
                (!model.setup.unfinished()).then(|| Err(error.message.clone()))
            }
        }
    }
}
