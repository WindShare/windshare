use serde::Deserialize;

#[derive(Clone, Debug, Deserialize)]
pub struct Snapshot {
    pub session_id: String,
    pub revision: u64,
    pub shares: Vec<Share>,
    #[serde(default)]
    pub setup: Setup,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct Share {
    pub operation_id: String,
    #[serde(default)]
    pub task_id: String,
    #[serde(flatten)]
    pub state: ShareState,
    pub startup: Startup,
    #[serde(default)]
    pub url: String,
    #[serde(default)]
    pub created_at_ms: u64,
    pub selection: Option<Selection>,
    pub result: Option<Completion>,
    pub error: Option<Failure>,
    pub relay: Option<RelayObservation>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct RelayObservation {
    pub payload: RelayAvailability,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct RelayAvailability {
    pub available: u32,
    pub total: u32,
    pub ever_ready: bool,
    pub terminal: u32,
}

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(tag = "state", rename_all = "snake_case")]
pub enum ShareState {
    AwaitingConfiguration { blocking: CreationBlock },
    AwaitingNetwork { blocking: CreationBlock },
    Admitting,
    Preparing,
    Sharing,
    Stopping,
    Finished,
}

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
pub struct CreationBlock {
    pub reason: BlockingReason,
    pub recovery: RecoveryAction,
    pub retry_at_ms: Option<u64>,
}

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum BlockingReason {
    SettingsUnavailable,
    ConfigurationRequired,
    Unprovisioned,
    InvalidNetworkConfiguration,
    TrustStateUnreadable,
    TrustStateInvalid,
    TrustStateUnwritable,
    DirectoryUnavailable,
    NoRelayCandidates,
    AliasUnavailable,
}

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum RecoveryAction {
    Retry,
    Configure,
    RepairSettings,
    RepairTrustState,
    ReplaceBuild,
}

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Startup {
    Pending,
    Ready,
    Failed,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct Completion {
    pub outcome: Outcome,
    pub stop_reason: StopReason,
    #[serde(default)]
    pub error: String,
    #[serde(default)]
    pub cleanup_error: String,
}

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum Outcome {
    Stopped,
    Cancelled,
    Failed,
}

#[derive(Clone, Copy, Debug, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum StopReason {
    None,
    Cancelled,
    ShareStopped,
    ApplicationClosed,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct Selection {
    pub count: u64,
    pub root: Option<SelectedRoot>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum SelectedRoot {
    File { name: String, file_size: u64 },
    Directory { name: String },
}

#[derive(Clone, Debug, Default, Deserialize, PartialEq, Eq)]
#[serde(tag = "state", rename_all = "snake_case")]
pub enum Setup {
    #[default]
    Idle,
    Running {
        id: String,
        #[serde(default)]
        operation_id: String,
        #[serde(default)]
        stage: String,
        #[serde(default)]
        verification_url: String,
        #[serde(default)]
        user_code: String,
    },
    Finished {
        id: String,
        #[serde(default)]
        operation_id: String,
    },
    Failed {
        id: String,
        #[serde(default)]
        operation_id: String,
        error: Failure,
    },
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct Failure {
    pub code: String,
    pub message: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Connection {
    Starting,
    Connected,
    Reconnecting(String),
    Closing,
    Closed(Result<(), String>),
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum OperationState {
    Submitting,
    Accepted(Box<Share>),
    Rejected(Failure),
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Operation {
    pub id: String,
    pub paths: Vec<String>,
    pub state: OperationState,
}

impl Operation {
    pub fn share(&self) -> Option<&Share> {
        match &self.state {
            OperationState::Accepted(share) => Some(share),
            _ => None,
        }
    }

    pub fn error(&self) -> Option<&str> {
        match &self.state {
            OperationState::Rejected(error) => Some(&error.message),
            OperationState::Accepted(share) => share
                .error
                .as_ref()
                .map(|error| error.message.as_str())
                .or_else(|| {
                    share.result.as_ref().and_then(|result| {
                        [&result.error, &result.cleanup_error]
                            .into_iter()
                            .find(|error| !error.is_empty())
                            .map(String::as_str)
                    })
                }),
            OperationState::Submitting => None,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Model {
    pub connection: Connection,
    pub session_id: Option<String>,
    pub revision: u64,
    pub operations: Vec<Operation>,
    pub command_error: Option<String>,
    pub setup: Setup,
}

impl Default for Model {
    fn default() -> Self {
        Self {
            connection: Connection::Starting,
            session_id: None,
            revision: 0,
            operations: Vec::new(),
            command_error: None,
            setup: Setup::Idle,
        }
    }
}

impl Model {
    pub fn reconcile(&mut self, snapshot: Snapshot) {
        if self
            .session_id
            .as_ref()
            .is_some_and(|session| session != &snapshot.session_id)
            || snapshot.revision <= self.revision
        {
            return;
        }
        self.session_id = Some(snapshot.session_id);
        self.revision = snapshot.revision;
        self.setup = snapshot.setup;
        // A full snapshot replaces accepted state, but cannot erase unsent local
        // intents whose commands have not yet reached the host.
        self.operations.retain(|operation| {
            !matches!(operation.state, OperationState::Accepted(_))
                || snapshot
                    .shares
                    .iter()
                    .any(|share| share.operation_id == operation.id)
        });
        for share in snapshot.shares {
            if let Some(operation) = self
                .operations
                .iter_mut()
                .find(|op| op.id == share.operation_id)
            {
                operation.state = OperationState::Accepted(Box::new(share));
            } else {
                self.operations.push(Operation {
                    id: share.operation_id.clone(),
                    paths: Vec::new(),
                    state: OperationState::Accepted(Box::new(share)),
                });
            }
        }
        // A snapshot may predate admission of queued creates. Keep those local
        // selections registered until their own reply or a newer snapshot arrives.
    }

    pub fn reject(&mut self, operation_id: &str, error: Failure) {
        if let Some(operation) = self.operations.iter_mut().find(|op| op.id == operation_id) {
            operation.state = OperationState::Rejected(error);
        }
    }
}

#[cfg(test)]
#[path = "model_tests.rs"]
mod tests;
