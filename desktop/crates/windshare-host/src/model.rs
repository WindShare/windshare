use crate::Setup;
use serde::Deserialize;
use std::{
    collections::{HashMap, HashSet},
    path::PathBuf,
};

#[derive(Clone, Debug, Deserialize)]
pub struct Snapshot {
    pub session_id: String,
    pub revision: u64,
    pub shares: Vec<Share>,
    #[serde(default)]
    pub setup: Setup,
    #[serde(default)]
    pub preferences: crate::RelayPreferences,
    pub account: Option<crate::AccountView>,
    #[serde(default)]
    pub sender_trace: SenderTrace,
}

#[derive(Clone, Copy, Debug, Default, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum SenderTrace {
    #[default]
    Unavailable,
    Off,
    On,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum SenderTraceChange {
    #[default]
    Idle,
    Applying,
    Failed,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct Share {
    pub operation_id: String,
    #[serde(default)]
    pub created_at_ms: u64,
    pub selection: Option<Selection>,
    #[serde(flatten)]
    pub state: ShareState,
}

impl Share {
    pub const fn is_finished(&self) -> bool {
        matches!(self.state, ShareState::Finished { .. })
    }

    pub fn error(&self) -> Option<&str> {
        match &self.state {
            ShareState::AwaitingConfiguration { error, .. }
            | ShareState::AwaitingNetwork { error, .. }
            | ShareState::Preparing { error, .. } => {
                error.as_ref().map(|failure| failure.message.as_str())
            }
            ShareState::Finished { result, error, .. } => error
                .as_ref()
                .map(|failure| failure.message.as_str())
                .or_else(|| {
                    [&result.error, &result.cleanup_error]
                        .into_iter()
                        .find(|message| !message.is_empty())
                        .map(String::as_str)
                }),
            ShareState::AwaitingAuthorization { .. }
            | ShareState::Admitting
            | ShareState::Sharing { .. }
            | ShareState::Stopping { .. } => None,
        }
    }
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum ShareAuthorization {
    SelfHosted { relay_url: String },
    Official,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct RelayAvailability {
    pub available: u32,
    pub total: u32,
    pub ever_ready: bool,
    pub terminal: u32,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RelayStatus {
    Available,
    Recovering,
    Unavailable,
}

impl RelayAvailability {
    pub const fn status(&self) -> RelayStatus {
        if self.available > 0 {
            RelayStatus::Available
        } else if self.total > 0 && self.terminal == self.total {
            RelayStatus::Unavailable
        } else {
            RelayStatus::Recovering
        }
    }
}

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
#[serde(tag = "state", rename_all = "snake_case")]
pub enum ShareState {
    AwaitingAuthorization {
        authorization: ShareAuthorization,
    },
    AwaitingConfiguration {
        blocking: CreationBlock,
        error: Option<Failure>,
    },
    AwaitingNetwork {
        blocking: CreationBlock,
        error: Option<Failure>,
    },
    Admitting,
    Preparing {
        task_id: String,
        error: Option<Failure>,
    },
    Sharing {
        task_id: String,
        url: String,
        #[serde(default)]
        backup_url: Option<String>,
        relay: Option<RelayAvailability>,
    },
    Stopping {
        #[serde(default)]
        task_id: Option<String>,
    },
    Finished {
        result: Completion,
        error: Option<Failure>,
        authorization: Option<ShareAuthorization>,
    },
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
    SelectedRelaysUnavailable,
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

#[derive(Clone, Debug, Deserialize, PartialEq, Eq)]
pub struct Failure {
    pub code: String,
    pub message: String,
}

impl From<String> for Failure {
    fn from(message: String) -> Self {
        Self {
            code: "request_rejected".into(),
            message,
        }
    }
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
    pub paths: Vec<std::path::PathBuf>,
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
            OperationState::Accepted(share) => share.error(),
            OperationState::Submitting => None,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Model {
    pub connection: Connection,
    pub session_id: Option<String>,
    pub log_directory: Option<PathBuf>,
    pub revision: u64,
    pub operations: Vec<Operation>,
    pub command_error: Option<String>,
    pub account_request: crate::AccountRequest,
    pub setup_request: crate::SetupRequest,
    pub sender_trace: SenderTrace,
    pub sender_trace_change: SenderTraceChange,
    pub setup: Setup,
    pub preferences: crate::RelayPreferences,
    pub account: Option<crate::AccountView>,
}

impl Default for Model {
    fn default() -> Self {
        Self {
            connection: Connection::Starting,
            session_id: None,
            log_directory: None,
            revision: 0,
            operations: Vec::new(),
            command_error: None,
            account_request: crate::AccountRequest::Idle,
            setup_request: crate::SetupRequest::Idle,
            sender_trace: SenderTrace::Unavailable,
            sender_trace_change: SenderTraceChange::Idle,
            setup: Setup::Idle,
            preferences: crate::RelayPreferences::default(),
            account: None,
        }
    }
}

impl Model {
    pub fn account_busy(&self) -> bool {
        self.account_request.is_pending()
            || self.account.as_ref().is_some_and(crate::AccountView::busy)
    }

    pub const fn setup_pending(&self) -> bool {
        self.setup_request.is_pending()
    }

    pub fn waits_for_official_authorization(&self, operation_id: &str) -> bool {
        self.operations
            .iter()
            .find(|operation| operation.id == operation_id)
            .and_then(Operation::share)
            .is_some_and(|share| {
                matches!(
                    share.state,
                    ShareState::AwaitingAuthorization {
                        authorization: ShareAuthorization::Official
                    }
                )
            })
    }

    pub fn has_unfinished_work(&self) -> bool {
        self.account_busy()
            || self.setup_pending()
            || self.setup.unfinished()
            || self
                .operations
                .iter()
                .any(|operation| match &operation.state {
                    OperationState::Submitting => true,
                    OperationState::Accepted(share) => !share.is_finished(),
                    OperationState::Rejected(_) => false,
                })
    }

    pub(crate) fn accepts_snapshot(&self, snapshot: &Snapshot) -> bool {
        self.session_id
            .as_ref()
            .is_none_or(|session| session == &snapshot.session_id)
            && snapshot.revision > self.revision
    }

    pub(crate) fn can_reject(&self, operation_id: &str, error: &Failure) -> bool {
        self.operations
            .iter()
            .find(|op| op.id == operation_id)
            .is_some_and(
                |op| !matches!(&op.state, OperationState::Rejected(existing) if existing == error),
            )
    }

    pub fn reconcile(&mut self, snapshot: Snapshot) {
        if !self.accepts_snapshot(&snapshot) {
            return;
        }
        self.session_id = Some(snapshot.session_id);
        self.revision = snapshot.revision;
        self.setup = snapshot.setup;
        self.preferences = snapshot.preferences;
        self.account = snapshot.account;
        self.sender_trace = snapshot.sender_trace;
        self.reconcile_account_request();
        self.reconcile_setup_request();
        // A full snapshot replaces accepted state, but cannot erase unsent local
        // intents whose commands have not yet reached the host.
        let present: HashSet<&str> = snapshot
            .shares
            .iter()
            .map(|share| share.operation_id.as_str())
            .collect();
        self.operations.retain(|operation| {
            !matches!(operation.state, OperationState::Accepted(_))
                || present.contains(operation.id.as_str())
        });
        drop(present);
        let mut index_by_id: HashMap<String, usize> = self
            .operations
            .iter()
            .enumerate()
            .map(|(index, op)| (op.id.clone(), index))
            .collect();
        for share in snapshot.shares {
            if let Some(&index) = index_by_id.get(&share.operation_id) {
                let operation = &mut self.operations[index];
                if let ShareState::Sharing { task_id, .. } = &share.state
                    && operation.share().is_none_or(|previous| {
                        !matches!(previous.state, ShareState::Sharing { .. })
                    })
                {
                    tracing::info!(event = "share.engine_ready", host_session = ?self.session_id,
                        intent = %share.operation_id, operation = %share.operation_id,
                        task = %task_id, "Host made the share link usable");
                }
                operation.state = OperationState::Accepted(Box::new(share));
            } else {
                index_by_id.insert(share.operation_id.clone(), self.operations.len());
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

    fn reconcile_account_request(&mut self) {
        let crate::AccountRequest::Pending { id } = &self.account_request else {
            return;
        };
        let Some(operation) = self
            .account
            .as_ref()
            .and_then(|view| view.operation.as_ref())
            .filter(|op| &op.id == id)
        else {
            return;
        };
        match &operation.outcome {
            crate::AccountOutcome::Running => {}
            crate::AccountOutcome::Succeeded => {
                self.account_request = crate::AccountRequest::Succeeded { id: id.clone() };
            }
            crate::AccountOutcome::Failed { error } => {
                self.account_request = crate::AccountRequest::Failed {
                    id: id.clone(),
                    error: error.clone(),
                };
            }
        }
    }

    fn reconcile_setup_request(&mut self) {
        let crate::SetupRequest::Pending { id, kind } = &mut self.setup_request else {
            return;
        };
        if self.setup.context().is_none_or(|ctx| &ctx.id != id) {
            return;
        }
        let settled = match (&self.setup, *kind) {
            (Setup::Finished { .. }, _) => Some(crate::SetupRequest::Finished { id: id.clone() }),
            (Setup::Cancelled { .. }, _) => Some(crate::SetupRequest::Cancelled { id: id.clone() }),
            (Setup::Cancelling { .. }, crate::SetupRequestKind::Cancel { acknowledged }) => {
                if !acknowledged {
                    *kind = crate::SetupRequestKind::Cancel { acknowledged: true };
                }
                None
            }
            (
                Setup::Failed { error, .. } | Setup::CancelFailed { error, .. },
                crate::SetupRequestKind::Configure,
            )
            | (
                Setup::CancelFailed { error, .. },
                crate::SetupRequestKind::Cancel { acknowledged: true },
            ) => Some(crate::SetupRequest::Failed {
                id: id.clone(),
                error: error.clone(),
            }),
            _ => None,
        };
        if let Some(next) = settled {
            self.setup_request = next;
        }
    }
}

#[cfg(test)]
#[path = "model_tests.rs"]
mod tests;
