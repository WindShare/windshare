mod abi;
mod account;
pub use account::{AccountCommand, AccountState, AccountStatus, AccountView, Allowance};
mod admission;
mod connection;
mod model;
mod request;
mod setup;
pub use model::ShareAuthorization;
pub use setup::{Setup, SetupContext, SetupRecovery};

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
    OperationState, Outcome, RecoveryAction, RelayStatus, SelectedRoot, Selection, Share,
    ShareState, Snapshot, Startup, StopReason,
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
    model: Mutex<Model>,
    closing: AtomicBool,
    updates: async_channel::Sender<()>,
    wake: polling::Poller,
}

impl Shared {
    fn update(&self, change: impl FnOnce(&mut Model)) {
        let mut model = self.model.lock().expect("model lock");
        let before = model.clone();
        change(&mut model);
        if *model != before {
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
            model: Mutex::new(Model::default()),
            closing: AtomicBool::new(false),
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
                state.update(|model| model.connection = Connection::Closed(result));
            })?;
        Ok(Self {
            shared,
            commands,
            sequence: Arc::new(AtomicU64::new(0)),
            worker: Arc::new(Mutex::new(Some(worker))),
            updates: receiver_updates,
        })
    }

    /// # Panics
    /// Panics if the application model mutex is poisoned.
    pub fn model(&self) -> Model {
        self.shared.model.lock().expect("model lock").clone()
    }

    /// Coalesced model invalidation for the GUI executor. Subscribe before reading.
    pub async fn changed(&self) {
        let _ = self.updates.recv().await;
    }

    fn command(&self, command: Command) {
        let error = if self.shared.closing.load(Ordering::Acquire) {
            Some("WindShare is closing".into())
        } else {
            command.validate().err().or_else(|| {
                self.commands
                    .try_send(command)
                    .err()
                    .map(|error| error.to_string())
            })
        };
        self.shared.update(|model| model.command_error = error);
        let _ = self.shared.wake.notify();
    }

    pub fn account(&self, account: AccountCommand) {
        self.command(Command::Account { account });
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

    pub fn configure(&self, operation_id: &str, relay_url: String, front_url: String) {
        self.command(Command::Configure {
            operation_id: operation_id.into(),
            setup: request::SetupInput {
                id: format!(
                    "setup:{}",
                    self.sequence.fetch_add(1, Ordering::Relaxed) + 1
                ),
                relay_url,
                front_url,
            },
        });
    }

    pub fn cancel_setup(&self, setup_id: &str) {
        self.command(Command::CancelSetup {
            setup_id: setup_id.into(),
        });
    }

    pub fn resume(&self, operation_id: &str) {
        self.command(Command::Resume {
            operation_id: operation_id.into(),
        });
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
            self.shared.update(|model| {
                model.connection = Connection::Closed(Err("host worker panicked".into()));
            });
        }
        match &self.model().connection {
            Connection::Closed(result) => result.clone(),
            _ => Err("host worker ended without a shutdown result".into()),
        }
    }
}
