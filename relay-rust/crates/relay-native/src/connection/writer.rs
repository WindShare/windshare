use crate::actor::Event;
use anyhow::Result;
use futures_util::{Sink, SinkExt};
use std::{sync::Arc, time::Instant};
use tokio::sync::{Notify, OwnedSemaphorePermit, mpsc, watch};
use tokio_tungstenite::tungstenite::Message;
use windshare_relay_core::lifecycle::ConnectionRef;

pub(super) struct Frame {
    attribution: windshare_relay_access::Attribution,
    bytes: Vec<u8>,
    _slot: OwnedSemaphorePermit,
    _bytes: Option<OwnedSemaphorePermit>,
    session: Option<windshare_relay_core::sessions::SessionKey>,
}

impl Frame {
    pub(super) const fn attributed(
        mut self,
        attribution: windshare_relay_access::Attribution,
    ) -> Self {
        self.attribution = attribution;
        self
    }
    pub(super) const fn new(
        bytes: Vec<u8>,
        slot: OwnedSemaphorePermit,
        byte_charge: Option<OwnedSemaphorePermit>,
        session: Option<windshare_relay_core::sessions::SessionKey>,
    ) -> Self {
        Self {
            bytes,
            attribution: windshare_relay_access::Attribution::Anonymous,
            _slot: slot,
            _bytes: byte_charge,
            session,
        }
    }
}

pub struct Writer {
    usage: crate::node::usage::Meter,
    ordered: mpsc::Receiver<Frame>,
    probes: watch::Receiver<Option<Vec<u8>>>,
}

impl Writer {
    pub(crate) fn with_usage(mut self, usage: crate::node::usage::Meter) -> Self {
        self.usage = usage;
        self
    }
    pub(super) fn new(
        ordered: mpsc::Receiver<Frame>,
        probes: watch::Receiver<Option<Vec<u8>>>,
    ) -> Self {
        Self {
            ordered,
            probes,
            usage: crate::node::usage::Meter::default(),
        }
    }

    pub async fn run<S>(
        mut self,
        mut sink: S,
        flush: Arc<Notify>,
        events: mpsc::Sender<Event>,
        connection: ConnectionRef,
        metrics: crate::node::Metrics,
    ) -> Result<()>
    where
        S: Sink<Message> + Unpin,
        S::Error: std::error::Error + Send + Sync + 'static,
    {
        let mut probes_open = true;
        loop {
            tokio::select! {
                biased;
                changed = self.probes.changed(), if probes_open => {
                    probes_open = changed.is_ok();
                    let bytes = self.probes.borrow_and_update().clone();
                    if probes_open && let Some(bytes) = bytes {
                        send(&mut sink, bytes).await?;
                    }
                }
                () = flush.notified() => {
                    sink.flush().await?;
                }
                frame = self.ordered.recv() => {
                    let Some(mut frame) = frame else {
                        // Keep the read half alive for the peer's close reply before
                        // abortive TCP teardown can discard unacknowledged final controls.
                        sink.close().await?;
                        return Ok(());
                    };
                    let start = Instant::now();
                    let bytes = frame.bytes.len() as u64;
                    let scope = frame.session.map(|_| self.usage.enter(frame.attribution));
                    send(&mut sink, std::mem::take(&mut frame.bytes)).await?;
                    drop(scope);
                    if let Some(session) = frame.session {
                        metrics.forwarded(session, bytes, start, Instant::now());
                    }
                    // Keep both charges through the in-flight socket wait. Removing
                    // a message from mpsc alone must not replenish writer capacity.
                    drop(frame);
                    events.send(Event::Writable(connection)).await?;
                }
            }
        }
    }
}

async fn send<S>(sink: &mut S, bytes: Vec<u8>) -> Result<()>
where
    S: Sink<Message> + Unpin,
    S::Error: std::error::Error + Send + Sync + 'static,
{
    // Backpressure is not a liveness failure. The kernel owns communication and
    // settlement deadlines; the supervisor separately bounds terminal draining.
    sink.send(Message::Binary(bytes.into())).await?;
    Ok(())
}
#[cfg(test)]
#[path = "writer_tests.rs"]
mod tests;
