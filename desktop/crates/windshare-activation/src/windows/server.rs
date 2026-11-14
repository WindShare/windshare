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
        let mut pipe = Pipe::listen(&scope.pipe, &scope.sid)?;
        let closing = Arc::new(AtomicBool::new(false));
        let stop = closing.clone();
        let worker = std::thread::Builder::new()
            .name("windshare-activation".into())
            .spawn(move || {
                while !stop.load(Ordering::Acquire) {
                    if pipe.accept(Instant::now() + ACCEPT_POLL).is_err() {
                        continue;
                    }
                    let deadline = Instant::now() + EXCHANGE_TIMEOUT;
                    if let Err(error) = serve(&pipe, deadline, &receive) {
                        tracing::warn!(event = "activation.connection_failed", %error);
                    }
                    drop(pipe);
                    match Pipe::listen(&scope.pipe, &scope.sid) {
                        Ok(next) => pipe = next,
                        Err(error) => {
                            tracing::error!(event = "activation.listener_failed", %error);
                            break;
                        }
                    }
                }
            })?;
        Ok(Self {
            closing,
            worker: Some(worker),
        })
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
