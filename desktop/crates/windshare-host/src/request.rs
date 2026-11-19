use serde::Serialize;

#[cfg(test)]
#[path = "request_tests.rs"]
mod tests;

#[derive(Serialize)]
#[serde(tag = "command", rename_all = "snake_case")]
pub enum Command {
    Preferences,
    StartAccountSync,
    Account {
        operation_id: String,
        account: crate::AccountCommand,
    },
    Create {
        operation_id: String,
        paths: Vec<String>,
    },
    Repeat {
        operation_id: String,
        source_operation_id: String,
    },
    Cancel {
        operation_id: String,
    },
    Stop {
        task_id: String,
    },
    Configure {
        operation_id: String,
        setup: SetupInput,
    },
    CancelSetup {
        setup_id: String,
    },
    Resume {
        operation_id: String,
    },
    UseSavedSettings {
        operation_id: String,
    },
    SwitchRelay {
        operation_id: String,
        relay_url: String,
    },
}

#[derive(Default, Serialize)]
pub struct SetupInput {
    pub id: String,
    pub relay_url: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub relay_operation: Option<crate::RelayOperation>,
    pub front_url: String,
    pub access_key: String,
    pub preference: Option<crate::PreferenceEdit>,
}

#[derive(Serialize)]
pub struct Request<C = Command> {
    pub id: String,
    #[serde(flatten)]
    pub command: C,
}

#[cfg_attr(test, derive(Debug))]
#[derive(Default)]
struct CommandLog<'a> {
    command: &'static str,
    action: Option<&'static str>,
    operation_id: Option<&'a str>,
    source_operation_id: Option<&'a str>,
    task_id: Option<&'a str>,
    setup_id: Option<&'a str>,
    path_count: Option<usize>,
}

impl Request {
    pub fn log_submission(&self, session_id: &str) {
        // The wire command carries credentials. Keep each diagnostic field explicit
        // so adding a wire field cannot silently put it in a support log.
        let fields = self.command.log_fields();
        tracing::info!(
            event = "desktop.host_request",
            %session_id,
            request_id = %self.id,
            command = fields.command,
            action = fields.action,
            operation_id = fields.operation_id,
            source_operation_id = fields.source_operation_id,
            task_id = fields.task_id,
            setup_id = fields.setup_id,
            path_count = fields.path_count,
        );
    }
}

impl Command {
    fn log_fields(&self) -> CommandLog<'_> {
        let mut fields = CommandLog::default();
        let command = match self {
            Self::Preferences => "preferences",
            Self::StartAccountSync => "start_account_sync",
            Self::Account {
                operation_id,
                account,
            } => {
                fields.operation_id = Some(operation_id);
                fields.action = Some(account.action());
                "account"
            }
            Self::Create {
                operation_id,
                paths,
            } => {
                fields.operation_id = Some(operation_id);
                fields.path_count = Some(paths.len());
                "create"
            }
            Self::Repeat {
                operation_id,
                source_operation_id,
            } => {
                fields.operation_id = Some(operation_id);
                fields.source_operation_id = Some(source_operation_id);
                "repeat"
            }
            Self::Cancel { operation_id } => {
                fields.operation_id = Some(operation_id);
                "cancel"
            }
            Self::Stop { task_id } => {
                fields.task_id = Some(task_id);
                "stop"
            }
            Self::Configure {
                operation_id,
                setup,
            } => {
                fields.operation_id = (!operation_id.is_empty()).then_some(operation_id.as_str());
                fields.setup_id = Some(&setup.id);
                fields.action = setup.preference.as_ref().map(crate::PreferenceEdit::action);
                "configure"
            }
            Self::CancelSetup { setup_id } => {
                fields.setup_id = Some(setup_id);
                "cancel_setup"
            }
            Self::Resume { operation_id } => {
                fields.operation_id = Some(operation_id);
                "resume"
            }
            Self::UseSavedSettings { operation_id } => {
                fields.operation_id = Some(operation_id);
                "use_saved_settings"
            }
            Self::SwitchRelay { operation_id, .. } => {
                fields.operation_id = Some(operation_id);
                "switch_relay"
            }
        };
        CommandLog { command, ..fields }
    }
}

const MAXIMUM_REQUEST_BYTES: usize = 256 * 1024;

impl Command {
    pub fn validate(&self) -> Result<(), String> {
        // Reserve the longest possible request ID before the worker assigns one.
        // An oversized selection must fail locally, not reconnect forever after
        // the host rejects the WebSocket frame.
        let request = Request {
            id: u64::MAX.to_string(),
            command: self,
        };
        let bytes = serde_json::to_vec(&request).map_err(|error| error.to_string())?;
        if bytes.len() > MAXIMUM_REQUEST_BYTES {
            return Err("Too many selected paths; share their containing folder instead".into());
        }
        Ok(())
    }
}
