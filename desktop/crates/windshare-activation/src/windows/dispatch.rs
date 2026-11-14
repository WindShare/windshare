use super::{
    Scope,
    pipe::Pipe,
    server::{ExchangeFailure, exchange},
};
use crate::{Activation, Phase, Reply, Request};
use std::{
    os::windows::process::CommandExt,
    path::Path,
    process::{Child, Command},
    time::{Duration, Instant},
};

pub const DISPATCH_TIMEOUT: Duration = Duration::from_secs(30);
const RECEIPT_TIMEOUT: Duration = Duration::from_secs(5);
const RETRY_INTERVAL: Duration = Duration::from_millis(75);
const CREATE_NO_WINDOW: u32 = 0x0800_0000;

/// Hands ownership to a short-lived process before returning to Explorer.
/// # Errors
/// Returns launch, capacity, or dispatcher receipt errors.
pub fn from_shell(executable: &Path, activation: &Activation) -> anyhow::Result<()> {
    crate::protocol::encode(activation).map_err(anyhow::Error::msg)?;
    let scope = Scope::current()?;
    let endpoint = uuid::Uuid::new_v4().to_string();
    let pipe = Pipe::listen(&scope.dispatch_pipe(&endpoint)?, &scope.sid)?;
    let mut child = Command::new(executable)
        .args(["--dispatch", &endpoint])
        .creation_flags(CREATE_NO_WINDOW)
        .spawn()?;
    let deadline = Instant::now() + RECEIPT_TIMEOUT;
    let result = (|| {
        pipe.accept(deadline)?;
        anyhow::ensure!(
            pipe.client_pid()? == child.id(),
            "Unexpected dispatcher process"
        );
        // The shell's user invocation passes foreground permission along the handoff.
        // SAFETY: the verified PID belongs to our live dispatcher process.
        let _ = unsafe {
            windows::Win32::UI::WindowsAndMessaging::AllowSetForegroundWindow(child.id())
        };
        pipe.send(activation, deadline)?;
        let received: bool = pipe.receive(deadline)?;
        anyhow::ensure!(received, "Dispatcher rejected the selection");
        pipe.send(&(), deadline)?;
        Ok(())
    })();
    if result.is_err() {
        let _ = child.kill();
        let _ = child.wait();
    }
    result
}

/// # Errors
/// Returns a receipt or application acceptance failure; the executable displays it.
pub fn receive(endpoint: &str) -> anyhow::Result<()> {
    let scope = Scope::current()?;
    let pipe = Pipe::connect(&scope.dispatch_pipe(endpoint)?)?;
    let deadline = Instant::now() + RECEIPT_TIMEOUT;
    let activation: Activation = pipe.receive(deadline)?;
    if let Activation::ShareSelection(intent) = &activation {
        intent.validate().map_err(anyhow::Error::msg)?;
    }
    pipe.send(&true, deadline)?;
    let _: () = pipe.receive(deadline)?;
    drop(pipe);
    tracing::info!(event = "activation.dispatcher_received", source = ?activation_source(&activation));
    deliver(&scope, &activation, &std::env::current_exe()?)
}

/// Reconnects preserve the intent and the application session that might own it.
/// # Errors
/// Returns an explicit failure if acceptance cannot be confirmed before the deadline.
pub fn deliver(scope: &Scope, activation: &Activation, executable: &Path) -> anyhow::Result<()> {
    let started = Instant::now();
    let deadline = started + DISPATCH_TIMEOUT;
    let mut instance = None;
    let mut child: Option<Child> = None;
    let mut may_be_accepted = false;
    loop {
        anyhow::ensure!(
            Instant::now() < deadline,
            "WindShare did not accept the selection in time. Retry from Explorer."
        );
        let request = instance.map_or(Request::Probe, |instance| Request::Activate {
            instance,
            activation: activation.clone(),
        });
        match exchange(scope, &request, deadline) {
            Ok(Reply::Accepted { instance: accepted }) => {
                anyhow::ensure!(instance == Some(accepted), "Unexpected application session");
                tracing::info!(event = "activation.application_accepted", instance = %accepted,
                    intent = ?activation_id(activation), elapsed_ms = started.elapsed().as_millis());
                return Ok(());
            }
            Ok(Reply::Status {
                instance: current,
                phase,
            }) => {
                if instance.is_some_and(|previous| previous != current) && may_be_accepted {
                    anyhow::bail!(
                        "WindShare restarted before confirming the selection. Retry from Explorer."
                    );
                }
                instance = (phase != Phase::Closing).then_some(current);
            }
            Ok(Reply::Closing) => {
                instance = None;
                may_be_accepted = false;
            }
            Ok(Reply::Starting) => {
                may_be_accepted = false;
            }
            Ok(Reply::Rejected { message }) => anyhow::bail!("{message}"),
            Err(failure) => {
                let error = match failure {
                    ExchangeFailure::NotConnected(error) => error,
                    ExchangeFailure::DeliveryUncertain(error) => {
                        may_be_accepted |= matches!(request, Request::Activate { .. });
                        error
                    }
                };
                tracing::debug!(event = "activation.reconnecting", %error, may_be_accepted, instance = ?instance);
                if let Some(owner) = scope.elect()? {
                    drop(owner);
                    anyhow::ensure!(
                        !may_be_accepted,
                        "WindShare exited before confirming the selection. Retry from Explorer."
                    );
                    if child
                        .as_mut()
                        .is_none_or(|child| child.try_wait().ok().flatten().is_some())
                    {
                        child = Some(
                            Command::new(executable)
                                .creation_flags(CREATE_NO_WINDOW)
                                .spawn()?,
                        );
                        tracing::info!(event = "activation.owner_launched", intent = ?activation_id(activation));
                    }
                    instance = None;
                }
            }
        }
        std::thread::sleep(RETRY_INTERVAL);
    }
}

const fn activation_source(activation: &Activation) -> Option<crate::EntrySource> {
    match activation {
        Activation::ShowWindow => None,
        Activation::ShareSelection(intent) => Some(intent.source),
    }
}

const fn activation_id(activation: &Activation) -> Option<crate::IntentId> {
    match activation {
        Activation::ShowWindow => None,
        Activation::ShareSelection(intent) => Some(intent.id),
    }
}
