use crate::{
    Command, Connection, Controller, Failure, MAXIMUM_OPERATIONS, Operation, OperationState,
};
use std::{
    path::PathBuf,
    sync::{Arc, atomic::Ordering},
};
use windshare_activation::{IntentId, ShareIntent};

impl Controller {
    /// Owns a selection once per deliberate user action, including local failures.
    /// # Errors
    /// Returns an identity conflict, closed application, or operation capacity error.
    /// # Panics
    /// Panics if the application model mutex is poisoned.
    pub fn create(&self, intent: ShareIntent) -> Result<String, String> {
        let id = intent.id.to_string();
        let source = intent.source;
        let count = intent.roots.len();
        let validation = intent.validate();
        let result = self.submit(id, intent.roots, |operation_id, paths| {
            validation?;
            let paths = paths
                .iter()
                .map(|path| {
                    path.to_str().map(str::to_owned).ok_or_else(|| {
                        "A selected path cannot be represented as Unicode".to_owned()
                    })
                })
                .collect::<Result<_, _>>()?;
            Ok(Command::Create {
                operation_id,
                paths,
            })
        });
        tracing::info!(event = "desktop.intent_owned", intent_id = %intent.id, ?source, count,
            accepted = result.is_ok());
        result
    }

    /// Repeats the retained selection while preserving the host's authorization context.
    /// # Errors
    /// Returns a missing selection, application lifetime, or capacity error.
    /// # Panics
    /// Panics if the application model mutex is poisoned.
    pub fn repeat(&self, source_operation_id: &str) -> Result<String, String> {
        let model = self.model();
        let source = model
            .operations
            .iter()
            .find(|operation| operation.id == source_operation_id)
            .ok_or("Source selection not found")?;
        let paths = source.paths.clone();
        if matches!(source.state, OperationState::Rejected(_)) {
            // Local validation or queue failure never created a host-side selection.
            return self.create(ShareIntent::new(
                windshare_activation::EntrySource::Repeat,
                paths,
            ));
        }
        self.submit(IntentId::new().to_string(), paths, |operation_id, _| {
            Ok(Command::Repeat {
                operation_id,
                source_operation_id: source_operation_id.into(),
            })
        })
    }

    fn submit(
        &self,
        id: String,
        paths: Vec<PathBuf>,
        make_command: impl FnOnce(String, &[PathBuf]) -> Result<Command, String>,
    ) -> Result<String, String> {
        let mut slot = self.shared.model.lock().expect("model lock");
        if self.shared.closing.load(Ordering::Acquire)
            || matches!(slot.connection, Connection::Closing | Connection::Closed(_))
        {
            return Err("WindShare is closing".into());
        }
        if let Some(existing) = slot.operations.iter().find(|op| op.id == id) {
            return if existing.paths == paths {
                Ok(id)
            } else {
                Err("This share intent was already used for a different selection".into())
            };
        }
        if slot.operations.len() >= MAXIMUM_OPERATIONS {
            return Err("Share limit reached; restart after stopping shares".into());
        }
        let state = match make_command(id.clone(), &paths).and_then(|command| {
            command.validate()?;
            self.commands
                .try_send(command)
                .map_err(|error| format!("Share queue unavailable: {error}"))
        }) {
            Ok(()) => OperationState::Submitting,
            Err(message) => OperationState::Rejected(Failure {
                code: "submission_failed".into(),
                message,
            }),
        };
        let submitted = matches!(state, OperationState::Submitting);
        if let OperationState::Rejected(failure) = &state {
            tracing::warn!(event = "desktop.share_submission_rejected", operation_id = %id,
                code = %failure.code, message = %failure.message);
        }
        // Reconciliation takes the same lock; a reply cannot outrun ownership.
        // Even an unsupported path remains visible, without lossy conversion.
        let model = Arc::make_mut(&mut slot);
        model.operations.push(Operation {
            id: id.clone(),
            paths,
            state,
        });
        model.command_error = None;
        drop(slot);
        self.shared.notify();
        if submitted {
            tracing::info!(event = "desktop.share_submitted", operation_id = %id);
        }
        Ok(id)
    }
}
