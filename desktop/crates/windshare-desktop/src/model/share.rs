use super::{Action, ShareAge, ShareContents};
use crate::{
    localization::{Language, Text},
    sharing::{ShareLink, SharePublication},
};
use windshare_ui::{Icon, Tone};

#[derive(Clone, Debug)]
pub enum ShareSource {
    Preview,
    Native { operation_id: String },
}

#[derive(Clone)]
pub enum ShareState {
    AwaitingAuthorization(windshare_host::ShareAuthorization),
    AwaitingConfiguration {
        blocking: windshare_host::CreationBlock,
        error: Option<String>,
    },
    AwaitingNetwork {
        blocking: windshare_host::CreationBlock,
        error: Option<String>,
    },
    Preparing {
        error: Option<String>,
    },
    Live {
        publication: SharePublication,
        relay_status: Option<windshare_host::RelayStatus>,
    },
    Stopping,
    Stopped {
        error: Option<String>,
    },
    Cancelled {
        error: Option<String>,
    },
    Failed {
        error: Option<String>,
        authorization: Option<windshare_host::ShareAuthorization>,
    },
}

#[derive(Clone)]
pub struct Share {
    pub id: usize,
    pub source: ShareSource,
    pub name: String,
    pub contents: ShareContents,
    pub size: Option<String>,
    pub age: ShareAge,
    pub tone: Tone,
    pub symbol: Icon,
    pub state: ShareState,
}

impl Share {
    pub const fn relay_notice(&self) -> Option<Text> {
        match self.state {
            ShareState::Live {
                relay_status: Some(windshare_host::RelayStatus::Recovering),
                ..
            } => Some(Text::RelayReconnecting),
            ShareState::Live {
                relay_status: Some(windshare_host::RelayStatus::Unavailable),
                ..
            } => Some(Text::RelayUnavailable),
            _ => None,
        }
    }

    pub fn error(&self) -> Option<&str> {
        match &self.state {
            ShareState::AwaitingConfiguration { error, .. }
            | ShareState::AwaitingNetwork { error, .. }
            | ShareState::Preparing { error }
            | ShareState::Stopped { error }
            | ShareState::Cancelled { error }
            | ShareState::Failed { error, .. } => error.as_deref(),
            ShareState::AwaitingAuthorization(_)
            | ShareState::Live { .. }
            | ShareState::Stopping => None,
        }
    }

    pub const fn authorization(&self) -> Option<&windshare_host::ShareAuthorization> {
        match &self.state {
            ShareState::AwaitingAuthorization(authorization) => Some(authorization),
            ShareState::Failed { authorization, .. } => authorization.as_ref(),
            _ => None,
        }
    }

    pub const fn live(&self) -> bool {
        matches!(self.state, ShareState::Live { .. })
    }

    pub const fn finished(&self) -> bool {
        matches!(
            self.state,
            ShareState::Stopped { .. } | ShareState::Cancelled { .. } | ShareState::Failed { .. }
        )
    }

    pub const fn live_publication(&self) -> Option<&SharePublication> {
        match &self.state {
            ShareState::Live { publication, .. } => Some(publication),
            _ => None,
        }
    }

    pub fn usable_link(&self, browser_trace: bool) -> Option<&ShareLink> {
        self.live_publication()
            .map(|publication| publication.primary(browser_trace))
    }

    pub fn is_traced(&self, browser_trace: bool) -> bool {
        self.live_publication()
            .is_some_and(|publication| publication.is_traced(browser_trace))
    }

    pub fn description(&self, locale: Language) -> String {
        let contents = self.contents.description(locale);
        self.size
            .as_ref()
            .map_or_else(|| contents.clone(), |size| format!("{contents} · {size}"))
    }

    pub const fn status(&self) -> (Text, Tone) {
        match self.state {
            ShareState::AwaitingAuthorization(_) => (Text::AuthorizationRequired, Tone::Peach),
            ShareState::AwaitingConfiguration { .. } => (Text::SharingSetup, Tone::Peach),
            ShareState::AwaitingNetwork { .. } => (Text::NetworkUnavailable, Tone::Peach),
            ShareState::Preparing { .. } => (Text::PreparingLink, Tone::Peach),
            ShareState::Live { .. } => (Text::Sharing, Tone::Mint),
            ShareState::Stopping => (Text::StoppingShare, Tone::Peach),
            ShareState::Stopped { .. } => (Text::ShareStopped, Tone::Neutral),
            ShareState::Cancelled { .. } => (Text::ShareCancelled, Tone::Neutral),
            ShareState::Failed { .. } => (Text::ShareFailed, Tone::Rose),
        }
    }

    pub const fn control(&self) -> (Text, Icon, Action) {
        match &self.state {
            ShareState::AwaitingAuthorization(authorization)
            | ShareState::Failed {
                authorization: Some(authorization),
                ..
            } => {
                let (label, icon) = match authorization {
                    windshare_host::ShareAuthorization::SelfHosted { .. } => {
                        (Text::ApproveDevice, Icon::Shield)
                    }
                    windshare_host::ShareAuthorization::Official => {
                        (Text::AuthorizationRequired, Icon::Settings)
                    }
                };
                (label, icon, Action::ConfigureShare(self.id))
            }
            ShareState::AwaitingConfiguration { .. } => (
                Text::SharingSetup,
                Icon::Settings,
                Action::ConfigureShare(self.id),
            ),
            ShareState::AwaitingNetwork { .. } => (
                Text::RetryConnection,
                Icon::Refresh,
                Action::ResumeShare(self.id),
            ),
            ShareState::Preparing { .. } => (Text::Cancel, Icon::Close, Action::StopShare(self.id)),
            ShareState::Live { .. } => (Text::StopSharing, Icon::Close, Action::StopShare(self.id)),
            ShareState::Stopping => (Text::StoppingShare, Icon::Close, Action::StopShare(self.id)),
            ShareState::Stopped { .. }
            | ShareState::Cancelled { .. }
            | ShareState::Failed {
                authorization: None,
                ..
            } => (Text::ShareAgain, Icon::Refresh, Action::ShareAgain(self.id)),
        }
    }
}
