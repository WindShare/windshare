use super::{Admission, Closure, channel};
use crate::{actor::Event, config::SOCKET_TIMEOUT, generation::Generation};
use anyhow::{Result, anyhow, bail};
use futures_util::{Stream, StreamExt};
use std::sync::Arc;
use tokio::{
    io::{AsyncRead, AsyncWrite},
    sync::{Notify, mpsc, oneshot, watch},
    task::JoinSet,
};
use tokio_tungstenite::{WebSocketStream, tungstenite::Message};
use windshare_relay_core::lifecycle::ConnectionRef;

/// The caller must configure the transport so dropping both halves also releases
/// its send buffers. TCP uses abortive close; a close request alone is not evidence.
pub async fn run<I>(
    socket: WebSocketStream<I>,
    events: mpsc::Sender<Event>,
    connection: ConnectionRef,
    mut shutdown: watch::Receiver<bool>,
    generation: Generation,
    admission: Arc<Admission>,
) -> Result<()>
where
    I: AsyncRead + AsyncWrite + Unpin + Send + 'static,
{
    let (outbox, writer, mut closure) = channel(connection);
    tokio::select! {
        result = events.send(Event::Connected(connection, outbox)) => {
            // SendError owns the rejected event. Do not let an Outbox escape
            // tracked cleanup inside a task's returned error value.
            result.map_err(|_| anyhow!("share actor unavailable"))?;
        }
        () = stopped(&mut shutdown) => return Ok(()),
        () = generation.cancelled() => return Ok(()),
    }
    let (sink, source) = socket.split();
    let flush = Arc::new(Notify::new());
    let mut tasks = JoinSet::new();
    let read_events = events.clone();
    let read_flush = Arc::clone(&flush);
    let read_admission = Arc::clone(&admission);
    let reader = tasks.spawn(generation.track(async move {
        read(source, read_events, connection, read_flush, read_admission).await
    }));
    let write_events = events.clone();
    let write_admission = Arc::clone(&admission);
    let writer = tasks.spawn(generation.track(async move {
        let _admission = write_admission;
        writer.run(sink, flush, write_events, connection).await?;
        Ok(Closure::Drain)
    }));
    tracing::debug!(event = "connection_tasks_started", ?connection);
    let (mode, result) = tokio::select! {
        biased;
        () = stopped(&mut shutdown) => (Closure::Abort, Ok(())),
        () = generation.cancelled() => (Closure::Abort, Ok(())),
        changed = closure.changed() => {
            let mode = if changed.is_ok() { *closure.borrow_and_update() } else { Closure::Abort };
            (mode, Ok(()))
        }
        completed = tasks.join_next() => outcome(completed),
    };
    tracing::debug!(event = "connection_closing", ?connection, ?mode);
    if mode != Closure::Drain {
        reader.abort();
        writer.abort();
    }
    let _ = events.send(Event::Disconnected(connection)).await;
    let cleanup = drain(&mut tasks, &mut shutdown, &generation, connection).await;
    // Joining, rather than dropping JoinHandles, proves the split transport,
    // in-flight frame and writer mailboxes have actually been destroyed.
    tracing::debug!(event = "connection_transport_released", ?connection);
    let (receipt, applied) = oneshot::channel();
    if events
        .send(Event::Closed(connection, receipt))
        .await
        .is_ok()
    {
        // Server shutdown retains actors until they have consumed socket release,
        // not merely until the receipt has entered a mailbox.
        let _ = applied.await;
    }
    drop(admission);
    result.and(cleanup)
}

async fn drain(
    tasks: &mut JoinSet<Result<Closure>>,
    shutdown: &mut watch::Receiver<bool>,
    generation: &Generation,
    connection: ConnectionRef,
) -> Result<()> {
    // One total deadline prevents terminal draining from being prolonged by
    // trickling progress; a server shutdown interrupts it immediately.
    tokio::select! {
        result = tokio::time::timeout(SOCKET_TIMEOUT, join(tasks)) => {
            if let Ok(result) = result {
                result
            } else {
                tracing::debug!(event = "connection_drain_expired", ?connection);
                tasks.abort_all();
                join(tasks).await
            }
        }
        () = stopped(shutdown) => {
            tasks.abort_all();
            join(tasks).await
        }
        () = generation.cancelled() => {
            tasks.abort_all();
            join(tasks).await
        }
    }
}

fn outcome(
    completed: Option<std::result::Result<Result<Closure>, tokio::task::JoinError>>,
) -> (Closure, Result<()>) {
    match completed.expect("two owned connection tasks") {
        Ok(Ok(mode)) => (mode, Ok(())),
        Ok(Err(error)) => (Closure::Abort, Err(error)),
        Err(error) => (Closure::Abort, Err(error.into())),
    }
}

async fn join(tasks: &mut JoinSet<Result<Closure>>) -> Result<()> {
    let mut result = Ok(());
    while let Some(completed) = tasks.join_next().await {
        match completed {
            Ok(Err(error)) => result = Err(error),
            Err(error) if !error.is_cancelled() => result = Err(error.into()),
            _ => {}
        }
    }
    result
}

pub async fn stopped(shutdown: &mut watch::Receiver<bool>) {
    while !*shutdown.borrow_and_update() {
        if shutdown.changed().await.is_err() {
            return;
        }
    }
}

async fn read<S>(
    mut source: S,
    events: mpsc::Sender<Event>,
    connection: ConnectionRef,
    flush: Arc<Notify>,
    admission: Arc<Admission>,
) -> Result<Closure>
where
    S: Stream<Item = std::result::Result<Message, tokio_tungstenite::tungstenite::Error>> + Unpin,
{
    let mut accepting = true;
    while let Some(message) = source.next().await {
        match message? {
            Message::Binary(bytes) if accepting => {
                if supported_control(&bytes) {
                    admission.inspect(&bytes, connection)?;
                    events
                        .send(Event::Control(connection, bytes.to_vec()))
                        .await
                        .map_err(|_| anyhow!("share actor unavailable"))?;
                } else {
                    events
                        .send(Event::Unsupported(connection))
                        .await
                        .map_err(|_| anyhow!("share actor unavailable"))?;
                    // Continue driving the close handshake without accepting more
                    // commands after this connection's capability refusal.
                    accepting = false;
                }
            }
            Message::Ping(_) => flush.notify_one(),
            Message::Pong(_) | Message::Binary(_) => {}
            Message::Close(_) => {
                flush.notify_one();
                return Ok(Closure::Drain);
            }
            _ => bail!("expected a binary control frame"),
        }
    }
    Ok(Closure::Abort)
}

fn supported_control(bytes: &[u8]) -> bool {
    // The kernel validates layouts and authentication. JOIN and opaque ingress
    // stay closed until forwarding and the client runtime are integrated.
    matches!(
        bytes.get(..4),
        Some(b"WS2R" | b"WS2P" | b"WS2U" | b"WS2T" | b"WS2X" | b"WS2V" | b"WS2H")
    )
}

#[cfg(test)]
#[path = "runtime_tests.rs"]
mod tests;
