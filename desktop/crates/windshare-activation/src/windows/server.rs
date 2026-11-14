use super::{Scope, pipe::Pipe};
use crate::{Reply, Request};
use std::{
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    thread::JoinHandle,
    time::{Duration, Instant},
};

#[cfg(test)]
#[path = "server_tests.rs"]
mod tests;

const ACCEPT_POLL: Duration = Duration::from_millis(100);
pub(super) const EXCHANGE_TIMEOUT: Duration = Duration::from_secs(2);

pub struct Server {
    closing: Arc<AtomicBool>,
    worker: Option<JoinHandle<()>>,
}

impl Server {
    /// # Errors
    /// Returns pipe or thread creation errors before the application starts accepting.
    pub fn start(
        scope: Scope,
        receive: impl Fn(Request) -> Reply + Send + 'static,
    ) -> anyhow::Result<Self> {
        let pipe = Pipe::listen(&scope.pipe, &scope.sid)?;
        let closing = Arc::new(AtomicBool::new(false));
        let stop = closing.clone();
        let worker = std::thread::Builder::new()
            .name("windshare-activation".into())
            .spawn(move || run(&scope, pipe, &stop, &receive))?;
        Ok(Self {
            closing,
            worker: Some(worker),
        })
    }
}

fn run(scope: &Scope, mut pipe: Pipe, stop: &AtomicBool, receive: &impl Fn(Request) -> Reply) {
    let span = tracing::info_span!("activation.listener", session = std::process::id(), pipe = %scope.pipe);
    let _entered = span.enter();
    while !stop.load(Ordering::Acquire) {
        match pipe.accept(Instant::now() + ACCEPT_POLL) {
            Ok(()) => {
                let deadline = Instant::now() + EXCHANGE_TIMEOUT;
                if let Err(error) = serve(&pipe, deadline, receive) {
                    tracing::warn!(event = "activation.connection_failed", %error);
                }
            }
            Err(error)
                if error.code() == windows::Win32::Foundation::ERROR_TIMEOUT.to_hresult() =>
            {
                continue;
            }
            Err(error) => tracing::warn!(event = "activation.accept_failed", %error),
        }
        // A disconnected client leaves the instance unusable even if accept failed.
        // Only an idle poll can safely retain it for the next connection.
        drop(pipe);
        match Pipe::listen(&scope.pipe, &scope.sid) {
            Ok(next) => pipe = next,
            Err(error) => {
                tracing::error!(event = "activation.listener_failed", %error);
                break;
            }
        }
    }
}

fn serve(
    pipe: &Pipe,
    deadline: Instant,
    receive: &impl Fn(Request) -> Reply,
) -> anyhow::Result<()> {
    let request = pipe.receive(deadline)?;
    pipe.send(&receive(request), deadline)?;
    // Wait only within this exchange's deadline; closing a server pipe before the
    // client reads its reply can otherwise discard the acknowledgement.
    let _: () = pipe.receive(deadline)?;
    Ok(())
}

impl Drop for Server {
    fn drop(&mut self) {
        self.closing.store(true, Ordering::Release);
        if let Some(worker) = self.worker.take() {
            let _ = worker.join();
        }
    }
}

#[derive(Debug)]
pub(super) enum ExchangeFailure {
    NotConnected(anyhow::Error),
    DeliveryUncertain(anyhow::Error),
}

pub(super) fn exchange(
    scope: &Scope,
    request: &Request,
    deadline: Instant,
) -> Result<Reply, ExchangeFailure> {
    let pipe = Pipe::connect(&scope.pipe).map_err(ExchangeFailure::NotConnected)?;
    if matches!(request, Request::Activate { .. }) {
        pipe.allow_server_foreground();
    }
    let deadline = deadline.min(Instant::now() + EXCHANGE_TIMEOUT);
    pipe.send(request, deadline)
        .map_err(ExchangeFailure::DeliveryUncertain)?;
    let reply = pipe
        .receive(deadline)
        .map_err(ExchangeFailure::DeliveryUncertain)?;
    let _ = pipe.send(&(), deadline);
    Ok(reply)
}
