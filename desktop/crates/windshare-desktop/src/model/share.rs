use super::{Action, ShareAge, ShareContents};
use crate::{
    localization::{Language, Text},
    sharing::ShareLink,
};
use windshare_ui::{Icon, Tone};

#[derive(Clone, Debug)]
pub enum ShareSource {
    Preview,
    Native { operation_id: String },
}

#[derive(Clone, Copy, Debug)]
pub enum ShareState {
    AwaitingAuthorization,
    AwaitingConfiguration(windshare_host::CreationBlock),
    AwaitingNetwork(windshare_host::CreationBlock),
    Preparing,
    Live { viewers: Option<usize> },
    Paused,
    Stopping,
    Stopped,
    Cancelled,
    Failed,
}

#[derive(Clone)]
pub struct Share {
    pub id: usize,
    pub source: ShareSource,
    pub name: String,
    pub link: Option<ShareLink>,
    pub contents: ShareContents,
    pub size: Option<String>,
    pub age: ShareAge,
    pub tone: Tone,
    pub symbol: Icon,
    pub state: ShareState,
    pub error: Option<String>,
    pub authorization: Option<windshare_host::ShareAuthorization>,
    pub relay_status: Option<windshare_host::RelayStatus>,
}

impl Share {
    pub const fn relay_notice(&self) -> Option<Text> {
        match self.relay_status {
            Some(windshare_host::RelayStatus::Recovering) => Some(Text::RelayReconnecting),
            Some(windshare_host::RelayStatus::Unavailable) => Some(Text::RelayUnavailable),
            _ => None,
        }
    }

    pub const fn live(&self) -> bool {
        matches!(self.state, ShareState::Live { .. })
    }

    pub const fn finished(&self) -> bool {
        matches!(
            self.state,
            ShareState::Paused | ShareState::Stopped | ShareState::Cancelled | ShareState::Failed
        )
    }

    pub fn usable_link(&self) -> Option<&ShareLink> {
        self.link.as_ref().filter(|_| self.live())
    }

    pub fn description(&self, locale: Language) -> String {
        let contents = self.contents.description(locale);
        self.size
            .as_ref()
            .map_or_else(|| contents.clone(), |size| format!("{contents} · {size}"))
    }

    pub const fn status(&self) -> (Text, Tone) {
        match self.state {
            ShareState::AwaitingAuthorization => (Text::AuthorizationRequired, Tone::Peach),
            ShareState::AwaitingConfiguration(_) => (Text::SharingSetup, Tone::Peach),
            ShareState::AwaitingNetwork(_) => (Text::NetworkUnavailable, Tone::Peach),
            ShareState::Preparing => (Text::PreparingLink, Tone::Peach),
            ShareState::Live { .. } => (Text::Sharing, Tone::Mint),
            ShareState::Paused => (Text::Paused, Tone::Neutral),
            ShareState::Stopping => (Text::StoppingShare, Tone::Peach),
            ShareState::Stopped => (Text::ShareStopped, Tone::Neutral),
            ShareState::Cancelled => (Text::ShareCancelled, Tone::Neutral),
            ShareState::Failed => (Text::ShareFailed, Tone::Rose),
        }
    }

    pub fn status_description(&self, locale: Language) -> String {
        match self.state {
            ShareState::Live {
                viewers: Some(count),
            } if count > 0 => locale.viewers(count),
            ShareState::Live { viewers: Some(_) } => locale.text(Text::WaitingReceiver).into(),
            _ => locale.text(self.status().0).into(),
        }
    }

    pub const fn control(&self) -> (Text, Icon, Action) {
        if self.authorization.is_some()
            && matches!(
                self.state,
                ShareState::AwaitingAuthorization | ShareState::Failed
            )
        {
            return (
                Text::AuthorizationRequired,
                Icon::Settings,
                Action::ConfigureShare(self.id),
            );
        }
        match self.source {
            ShareSource::Preview => {
                if self.live() {
                    (Text::PauseShare, Icon::Pause, Action::ToggleShare(self.id))
                } else {
                    (Text::ResumeShare, Icon::Play, Action::ToggleShare(self.id))
                }
            }
            ShareSource::Native { .. } => match self.state {
                ShareState::AwaitingConfiguration(_) => (
                    Text::SharingSetup,
                    Icon::Settings,
                    Action::ConfigureShare(self.id),
                ),
                ShareState::AwaitingNetwork(_) => (
                    Text::RetryConnection,
                    Icon::Refresh,
                    Action::ResumeShare(self.id),
                ),
                ShareState::Preparing => (Text::Cancel, Icon::Close, Action::StopShare(self.id)),
                ShareState::Live { .. } | ShareState::Stopping => {
                    (Text::StopSharing, Icon::Close, Action::StopShare(self.id))
                }
                _ => (Text::ShareAgain, Icon::Refresh, Action::ShareAgain(self.id)),
            },
        }
    }
}
