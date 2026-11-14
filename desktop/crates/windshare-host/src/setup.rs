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
pub struct SetupContext {
    pub id: String,
    #[serde(default)]
    pub operation_id: String,
    #[serde(default)]
    pub frontend_saved: bool,
    #[serde(default)]
    pub relay_saved: bool,
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
