mod abi;
mod connection;
mod model;
mod request;

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
    Completion, Connection, Failure, Model, Operation, OperationState, Outcome, SelectedRoot,
    Selection, Setup, Share, ShareState, Snapshot, Startup, StopReason,
};

use request::Command;
use std::{
    path::PathBuf,
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

    /// Registers a selection before sending it; reconnects reuse this identity.
    /// No filesystem traversal or metadata lookup runs on the caller's thread.
    /// # Errors
    /// Returns a selection, application lifetime, or queue capacity error.
    /// # Panics
    /// Panics if the application model mutex is poisoned.
    pub fn create(&self, paths: Vec<PathBuf>) -> Result<String, String> {
        if paths.is_empty() {
            return Err("Select at least one file or folder".into());
        }
        let paths: Vec<String> = paths
            .into_iter()
            .map(|path| {
                path.into_os_string()
                    .into_string()
                    .map_err(|_| "A selected path cannot be represented as Unicode".to_owned())
            })
            .collect::<Result<_, _>>()?;
        let mut model = self.shared.model.lock().expect("model lock");
        if self.shared.closing.load(Ordering::Acquire)
            || matches!(model.connection, Connection::Closed(_))
        {
            return Err("WindShare is closing".into());
        }
        if model.operations.len() >= MAXIMUM_OPERATIONS {
            return Err("Share limit reached; restart after stopping shares".into());
        }
        let id = format!(
            "share:{}",
            self.sequence.fetch_add(1, Ordering::Relaxed) + 1
        );
        let command = Command::Create {
            operation_id: id.clone(),
            paths: paths.clone(),
        };
        command.validate()?;
        self.commands
            .try_send(command)
            .map_err(|error| error.to_string())?;
        // The worker also takes this lock before reconciliation, so it cannot
        // publish a reply before the application owns the selection.
        model.operations.push(Operation {
            id: id.clone(),
            paths,
            state: OperationState::Submitting,
        });
        model.command_error = None;
        drop(model);
        self.shared.notify();
        tracing::info!(event = "desktop.share_submitted", operation_id = %id);
        Ok(id)
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

    pub fn resume(&self, operation_id: &str) {
        self.command(Command::Resume {
            operation_id: operation_id.into(),
        });
    }

    pub fn begin_shutdown(&self) {
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
