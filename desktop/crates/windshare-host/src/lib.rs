mod abi;
mod account;
pub use account::{
    AccountCommand, AccountOutcome, AccountProfile, AccountRequest, AccountState, AccountStatus,
    AccountView, Allowance, Nickname,
};
mod admission;
mod connection;
mod model;
mod request;
mod setup;
pub use model::ShareAuthorization;
pub use setup::{
    PreferenceEdit, RelayMode, RelayOperation, RelayPreferences, SavedRelay, Setup, SetupContext,
    SetupRecovery, SetupRequest, SetupRequestKind,
};

#[cfg(test)]
#[path = "lifecycle_tests.rs"]
mod lifecycle_tests;
#[cfg(test)]
#[path = "controller_tests.rs"]
mod tests;

/// Checks the packaged C ABI, authenticated control socket, and joined shutdown.
/// # Errors
/// Returns a build, ABI, socket, or engine shutdown failure.
pub fn check_host() -> anyhow::Result<()> {
    connection::check_host()
}

pub use model::{
    BlockingReason, Completion, Connection, CreationBlock, Failure, Model, Operation,
    OperationState, Outcome, RecoveryAction, RelayAvailability, RelayStatus, SelectedRoot,
    Selection, SenderTrace, SenderTraceChange, Share, ShareState, Snapshot, StopReason,
};

use request::Command;
use std::{
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, AtomicU64, Ordering},
        mpsc::{self, SyncSender},
    },
    thread::{self, JoinHandle},
};

const COMMAND_QUEUE_CAPACITY: usize = 32;
const MAXIMUM_OPERATIONS: usize = 1024;

struct Shared {
    model: Mutex<Arc<Model>>,
    closing: AtomicBool,
    account_view_visible: AtomicBool,
    updates: async_channel::Sender<()>,
    wake: polling::Poller,
}

/// Defers copy-on-write cloning until a state transition actually mutates the model.
struct ModelDraft<'a> {
    slot: &'a mut Arc<Model>,
    dirty: bool,
}

impl ModelDraft<'_> {
    fn read(&self) -> &Model {
        self.slot
    }

    fn write(&mut self) -> &mut Model {
        self.dirty = true;
        Arc::make_mut(self.slot)
    }

    fn set_connection(&mut self, connection: Connection) {
        if self.read().connection != connection {
            self.write().connection = connection;
        }
    }

    fn set_command_error(&mut self, error: Option<String>) {
        if self.read().command_error != error {
            self.write().command_error = error;
        }
    }
}

impl Shared {
    fn update(&self, change: impl FnOnce(&mut ModelDraft<'_>)) {
        let mut slot = self.model.lock().expect("model lock");
        let mut draft = ModelDraft {
            slot: &mut slot,
            dirty: false,
        };
        change(&mut draft);
        let dirty = draft.dirty;
        drop(slot);
        if dirty {
            let _ = self.updates.try_send(());
        }
    }

    fn notify(&self) {
        let _ = self.updates.try_send(());
        let _ = self.wake.notify();
    }
}

#[derive(Clone)]
pub struct Controller {
    shared: Arc<Shared>,
    commands: SyncSender<Command>,
    sequence: Arc<AtomicU64>,
    worker: Arc<Mutex<Option<JoinHandle<()>>>>,
    updates: async_channel::Receiver<()>,
}

impl Controller {
    /// Starts one application worker without creating a share.
    /// # Errors
    /// Returns the OS error if the worker thread cannot be created.
    /// # Panics
    /// The worker panics if its application model mutex is poisoned.
    pub fn start() -> std::io::Result<Self> {
        let (updates, receiver_updates) = async_channel::bounded(1);
        let shared = Arc::new(Shared {
            model: Mutex::new(Arc::new(Model::default())),
            closing: AtomicBool::new(false),
            account_view_visible: AtomicBool::new(false),
            updates,
            wake: polling::Poller::new()?,
        });
        let (commands, receiver) = mpsc::sync_channel(COMMAND_QUEUE_CAPACITY);
        let state = shared.clone();
        let worker = thread::Builder::new()
            .name("windshare-host".into())
            .spawn(move || {
                let result =
                    connection::run(&state, &receiver).map_err(|error| format!("{error:#}"));
                state.update(|draft| draft.set_connection(Connection::Closed(result)));
            })?;
        Ok(Self {
            shared,
            commands,
            sequence: Arc::new(AtomicU64::new(0)),
            worker: Arc::new(Mutex::new(Some(worker))),
            updates: receiver_updates,
        })
    }

    /// Returns a shared immutable snapshot of the current host model.
    /// # Panics
    /// Panics if the application model mutex is poisoned.
    pub fn model(&self) -> Arc<Model> {
        Arc::clone(&self.shared.model.lock().expect("model lock"))
    }

    /// Coalesced model invalidation for the GUI executor. Subscribe before reading.
    pub async fn changed(&self) {
        let _ = self.updates.recv().await;
    }

    fn enqueue(&self, command: Command) -> Result<(), String> {
        if self.shared.closing.load(Ordering::Acquire) {
            return Err("WindShare is closing".into());
        }
        command.validate()?;
        self.commands
            .try_send(command)
            .map_err(|error| error.to_string())
    }

    fn command(&self, command: Command) -> bool {
        let error = self.enqueue(command).err();
        let queued = error.is_none();
        self.shared.update(|draft| draft.set_command_error(error));
        let _ = self.shared.wake.notify();
        queued
    }

    fn operation_id(&self, kind: &str) -> String {
        let sequence = self.sequence.fetch_add(1, Ordering::Relaxed) + 1;
        format!("{kind}:{sequence}")
    }

    pub fn reload_account_selection(&self) {
        self.command(Command::ReloadAccountSelection);
    }

    pub fn set_account_view_visible(&self, visible: bool) {
        // Avoid an atomic read-modify-write on steady-state renders while still
        // guaranteeing reconnects observe the latest visibility flag.
        if self.shared.account_view_visible.load(Ordering::Acquire) == visible {
            return;
        }
        if self
            .shared
            .account_view_visible
            .swap(visible, Ordering::AcqRel)
            != visible
        {
            let _ = self.shared.wake.notify();
        }
    }

    pub fn account(&self, account: AccountCommand) -> Option<String> {
        let id = self.operation_id("account");
        let mut queued = false;
        self.shared.update(|draft| {
            // Keep the model lock while recording the pending request so a fast
            // host reply cannot arrive before the UI observes the in-flight state.
            let next = match self.enqueue(Command::Account {
                operation_id: id.clone(),
                account,
            }) {
                Ok(()) => {
                    queued = true;
                    AccountRequest::Pending { id: id.clone() }
                }
                Err(message) => AccountRequest::Failed {
                    id: id.clone(),
                    error: Failure::from(message),
                },
            };
            draft.write().account_request = next;
        });
        let _ = self.shared.wake.notify();
        queued.then_some(id)
    }

    pub fn cancel(&self, operation_id: &str) {
        self.command(Command::Cancel {
            operation_id: operation_id.into(),
        });
    }

    pub fn stop(&self, task_id: &str) {
        self.command(Command::Stop {
            task_id: task_id.into(),
        });
    }

    pub fn refresh_preferences(&self) {
        self.command(Command::Preferences);
    }

    pub fn set_sender_trace(&self, enabled: bool) {
        self.shared.update(|draft| {
            let model = draft.read();
            if model.connection != Connection::Connected
                || model.sender_trace == SenderTrace::Unavailable
                || model.sender_trace_change == SenderTraceChange::Applying
                || enabled == (model.sender_trace == SenderTrace::On)
            {
                return;
            }
            // Keep the model lock until the pending state is installed so an
            // acknowledgement cannot arrive first and leave the control stuck.
            let next = match self.enqueue(Command::SetSenderTrace {
                sender_trace: enabled,
            }) {
                Ok(()) => SenderTraceChange::Applying,
                Err(error) => {
                    tracing::warn!(event = "desktop.sender_trace_queue_failed",
                        session_id = ?model.session_id, enabled, %error);
                    SenderTraceChange::Failed
                }
            };
            if model.sender_trace_change != next {
                draft.write().sender_trace_change = next;
            }
        });
        let _ = self.shared.wake.notify();
    }

    pub fn edit_preference(&self, operation_id: &str, edit: PreferenceEdit) -> Option<String> {
        self.submit_setup(
            operation_id,
            request::SetupInput {
                preference: Some(edit),
                ..Default::default()
            },
        )
    }

    pub fn setup_relay(
        &self,
        operation_id: &str,
        relay_operation: RelayOperation,
        relay_url: String,
        access_key: String,
    ) -> Option<String> {
        self.submit_setup(
            operation_id,
            request::SetupInput {
                relay_url,
                relay_operation: Some(relay_operation),
                access_key,
                ..Default::default()
            },
        )
    }

    fn submit_setup(&self, operation_id: &str, input: request::SetupInput) -> Option<String> {
        let id = self.operation_id("setup");
        let mut queued = false;
        self.shared.update(|draft| {
            let next = match self.enqueue(Command::Configure {
                operation_id: operation_id.into(),
                setup: request::SetupInput {
                    id: id.clone(),
                    ..input
                },
            }) {
                Ok(()) => {
                    queued = true;
                    SetupRequest::Pending {
                        id: id.clone(),
                        kind: SetupRequestKind::Configure,
                    }
                }
                Err(message) => SetupRequest::Failed {
                    id: id.clone(),
                    error: Failure::from(message),
                },
            };
            draft.write().setup_request = next;
        });
        let _ = self.shared.wake.notify();
        queued.then_some(id)
    }

    pub fn cancel_setup(&self, setup_id: &str) {
        let id = setup_id.to_owned();
        let mut queued = false;
        self.shared.update(|draft| {
            let model = draft.read();
            if !model.can_cancel_setup(&id) {
                tracing::debug!(
                    event = "desktop.setup_cancel_ignored",
                    session_id = ?model.session_id,
                    setup_id = %id,
                    setup_state = model.setup.state_name(),
                    current_setup_id = model.setup.context().map(|ctx| ctx.id.as_str()),
                );
                return;
            }
            let next = match self.enqueue(Command::CancelSetup {
                setup_id: id.clone(),
            }) {
                Ok(()) => {
                    queued = true;
                    SetupRequest::Pending {
                        id,
                        kind: SetupRequestKind::Cancel {
                            acknowledged: false,
                        },
                    }
                }
                Err(message) => SetupRequest::Failed {
                    id,
                    error: Failure::from(message),
                },
            };
            draft.write().setup_request = next;
        });
        if queued {
            let _ = self.shared.wake.notify();
        }
    }

    pub fn resume(&self, operation_id: &str) {
        self.command(Command::Resume {
            operation_id: operation_id.into(),
        });
    }

    pub fn use_saved_settings(&self, operation_id: &str) {
        self.command(Command::UseSavedSettings {
            operation_id: operation_id.into(),
        });
    }

    pub fn switch_relay(&self, operation_id: &str, relay_url: &str) -> bool {
        self.command(Command::SwitchRelay {
            operation_id: operation_id.into(),
            relay_url: relay_url.into(),
        })
    }

    /// # Panics
    /// Panics if the application model mutex is poisoned.
    pub fn begin_shutdown(&self) {
        let _admission = self.shared.model.lock().expect("model lock");
        self.shared.closing.store(true, Ordering::Release);
        let _ = self.shared.wake.notify();
    }

    /// Must be called off the GUI executor. Main also joins after the event loop.
    /// # Panics
    /// Panics if a worker or application model mutex is poisoned.
    /// # Errors
    /// Returns the retained startup, shutdown, or worker failure after joining.
    pub fn join(&self) -> Result<(), String> {
        self.begin_shutdown();
        let mut guard = self.worker.lock().expect("worker lock");
        let failed = guard.take().is_some_and(|worker| worker.join().is_err());
        drop(guard);
        if failed {
            self.shared.update(|draft| {
                draft.set_connection(Connection::Closed(Err("host worker panicked".into())));
            });
        }
        match &self.model().connection {
            Connection::Closed(result) => result.clone(),
            _ => Err("host worker ended without a shutdown result".into()),
        }
    }
}
