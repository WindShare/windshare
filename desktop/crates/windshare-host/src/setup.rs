use serde::Deserialize;

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Eq)]
#[serde(tag = "state", rename_all = "snake_case")]
pub enum Setup {
    #[default]
    Idle,
    Running {
        #[serde(flatten)]
        context: SetupContext,
        #[serde(default)]
        stage: String,
        #[serde(default)]
        verification_url: String,
        #[serde(default)]
        user_code: String,
        service_login: Option<ServiceLogin>,
        #[serde(default)]
        guest_available: bool,
    },
    Finished {
        #[serde(flatten)]
        context: SetupContext,
    },
    Failed {
        #[serde(flatten)]
        context: SetupContext,
        recovery: Option<SetupRecovery>,
        error: crate::Failure,
    },
    Cancelling {
        #[serde(flatten)]
        context: SetupContext,
    },
    Cancelled {
        #[serde(flatten)]
        context: SetupContext,
    },
    CancelFailed {
        #[serde(flatten)]
        context: SetupContext,
        error: crate::Failure,
    },
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct ServiceLogin {
    pub url: String,
    pub host: String,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct SetupContext {
    pub id: String,
    #[serde(default)]
    pub operation_id: String,
    #[serde(default)]
    pub frontend_saved: bool,
    #[serde(default)]
    pub relay_url: String,
    pub relay_operation: Option<RelayOperation>,
    #[serde(default)]
    pub relay_saved: bool,
    #[serde(default)]
    pub relay_removed: bool,
}

#[derive(Clone, Copy, Debug, Deserialize, serde::Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum RelayOperation {
    #[serde(rename = "add")]
    AddProfile,
    AuthorizeDevice,
}

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum SetupRecovery {
    Unknown,
    Restart,
    Continue,
    Cancel,
    RetryCancel,
    Blocked,
}

impl Setup {
    pub(crate) const fn state_name(&self) -> &'static str {
        match self {
            Self::Idle => "idle",
            Self::Running { .. } => "running",
            Self::Finished { .. } => "finished",
            Self::Failed { .. } => "failed",
            Self::Cancelling { .. } => "cancelling",
            Self::Cancelled { .. } => "cancelled",
            Self::CancelFailed { .. } => "cancel_failed",
        }
    }

    pub const fn context(&self) -> Option<&SetupContext> {
        match self {
            Self::Idle => None,
            Self::Running { context, .. }
            | Self::Finished { context }
            | Self::Failed { context, .. }
            | Self::Cancelling { context }
            | Self::Cancelled { context }
            | Self::CancelFailed { context, .. } => Some(context),
        }
    }

    pub const fn busy(&self) -> bool {
        matches!(self, Self::Running { .. } | Self::Cancelling { .. })
    }

    pub const fn unfinished(&self) -> bool {
        match self {
            Self::Running { .. } | Self::Cancelling { .. } | Self::CancelFailed { .. } => true,
            Self::Failed { recovery, .. } => !matches!(
                recovery,
                Some(SetupRecovery::Restart | SetupRecovery::Blocked)
            ),
            _ => false,
        }
    }

    pub fn can_configure(&self, operation: &str) -> bool {
        !self.busy()
            && !self.requires_cancellation()
            && (!self.unfinished()
                || self
                    .context()
                    .is_some_and(|context| context.operation_id == operation))
    }

    pub const fn requires_cancellation(&self) -> bool {
        matches!(
            self,
            Self::Cancelling { .. }
                | Self::CancelFailed { .. }
                | Self::Failed {
                    recovery: Some(SetupRecovery::Cancel | SetupRecovery::RetryCancel),
                    ..
                }
        )
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SetupRequestKind {
    Configure,
    Guest,
    Cancel { acknowledged: bool },
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub enum SetupRequest {
    #[default]
    Idle,
    Pending {
        id: String,
        operation_id: String,
        kind: SetupRequestKind,
    },
    /// Local enqueue failure or synchronous host command rejection that never
    /// transitioned `Model::setup` on the host.
    Rejected { id: String, error: crate::Failure },
}

/// Authoritative settlement outcome for a tracked setup ID, reconciling the
/// local command overlay (`SetupRequest`) with host `Setup` snapshots.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SetupSettlement<'a> {
    Finished,
    Cancelled,
    Failed(&'a crate::Failure),
}

impl SetupRequest {
    pub const fn is_pending(&self) -> bool {
        matches!(self, Self::Pending { .. })
    }
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct RelayPreferences {
    pub mode: RelayMode,
    #[serde(default = "enabled_by_default")]
    pub community_nodes: bool,
    #[serde(default)]
    pub community_notice: u64,
    pub frontend: String,
    #[serde(default)]
    pub custom_frontend: bool,
    pub relays: Vec<SavedRelay>,
    pub at_capacity: bool,
}

impl Default for RelayPreferences {
    fn default() -> Self {
        Self {
            mode: RelayMode::Automatic,
            community_nodes: true,
            community_notice: 0,
            frontend: String::new(),
            custom_frontend: false,
            relays: Vec::new(),
            at_capacity: false,
        }
    }
}

const fn enabled_by_default() -> bool {
    true
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct SavedRelay {
    pub url: String,
    pub enabled: bool,
    pub console_url: Option<String>,
    pub offered_frontend: Option<String>,
}

#[derive(Clone, Copy, Debug, Default, Deserialize, serde::Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum RelayMode {
    #[default]
    Automatic,
    Selected,
}

#[derive(Clone, serde::Serialize)]
#[serde(tag = "action", content = "value", rename_all = "snake_case")]
pub enum PreferenceEdit {
    Mode(RelayMode),
    #[serde(serialize_with = "serialize_on_off")]
    CommunityNodes(bool),
    Enable(String),
    Disable(String),
    Remove(String),
    Frontend(String),
}

// serde's `serialize_with` contract always passes the field by reference.
#[allow(clippy::trivially_copy_pass_by_ref)]
fn serialize_on_off<S: serde::Serializer>(
    enabled: &bool,
    serializer: S,
) -> Result<S::Ok, S::Error> {
    serializer.serialize_str(if *enabled { "on" } else { "off" })
}

impl PreferenceEdit {
    pub(crate) const fn action(&self) -> &'static str {
        match self {
            Self::Mode(_) => "mode",
            Self::CommunityNodes(_) => "community_nodes",
            Self::Enable(_) => "enable",
            Self::Disable(_) => "disable",
            Self::Remove(_) => "remove",
            Self::Frontend(_) => "frontend",
        }
    }
}
