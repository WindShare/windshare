use super::*;
use crate::{config::CONTROL_QUEUE_FRAMES, connection::Outbox, test_support::connection};
use futures_util::SinkExt;
use std::{
    io,
    pin::Pin,
    sync::atomic::{AtomicBool, Ordering},
    task::{Context, Poll},
};
use tokio::io::{DuplexStream, ReadBuf};
use tokio_tungstenite::tungstenite::protocol::{Role, WebSocketConfig};

const PIPE_BYTES: usize = 64;
const BLOCKED_FRAME_BYTES: usize = 256;

struct TrackedIo {
    stream: DuplexStream,
    blocked: Arc<Notify>,
    dropped: Arc<AtomicBool>,
}
impl Drop for TrackedIo {
    fn drop(&mut self) {
        self.dropped.store(true, Ordering::Release);
    }
}
impl AsyncRead for TrackedIo {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        Pin::new(&mut self.stream).poll_read(cx, buf)
    }
}
impl AsyncWrite for TrackedIo {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<io::Result<usize>> {
        let result = Pin::new(&mut self.stream).poll_write(cx, buf);
        if result.is_pending() {
            self.blocked.notify_one();
        }
        result
    }
    fn poll_flush(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.stream).poll_flush(cx)
    }
    fn poll_shutdown(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<io::Result<()>> {
        Pin::new(&mut self.stream).poll_shutdown(cx)
    }
}

struct Harness {
    client: WebSocketStream<DuplexStream>,
    events: mpsc::Receiver<Event>,
    shutdown: watch::Sender<bool>,
    task: tokio::task::JoinHandle<Result<()>>,
    blocked: Arc<Notify>,
    dropped: Arc<AtomicBool>,
    _directory: tempfile::TempDir,
}

impl Harness {
    async fn new() -> (Self, Outbox) {
        let (server, client) = tokio::io::duplex(PIPE_BYTES);
        let blocked = Arc::new(Notify::new());
        let dropped = Arc::new(AtomicBool::new(false));
        let server = WebSocketStream::from_raw_socket(
            TrackedIo {
                stream: server,
                blocked: Arc::clone(&blocked),
                dropped: Arc::clone(&dropped),
            },
            Role::Server,
            Some(WebSocketConfig::default().write_buffer_size(0)),
        )
        .await;
        let client = WebSocketStream::from_raw_socket(client, Role::Client, None).await;
        let (tx, mut events) = mpsc::channel(16);
        let (shutdown, stopping) = watch::channel(false);
        let fixture = crate::test_support::Fixture::new();
        let directory = tempfile::tempdir().unwrap();
        let database = crate::storage::Database::open(
            &directory.path().join("authority.sqlite"),
            fixture.identity().relay,
        )
        .unwrap();
        let generation = Generation::new(fixture.identity(), database);
        let task = tokio::spawn(generation.track(run(
            server,
            tx,
            connection(1),
            stopping,
            generation.clone(),
        )));
        let Event::Connected(_, port) = events.recv().await.unwrap() else {
            panic!("connection admission")
        };
        (
            Self {
                client,
                events,
                shutdown,
                task,
                blocked,
                dropped,
                _directory: directory,
            },
            port,
        )
    }

    async fn released(&mut self) -> Result<()> {
        let mut disconnected = false;
        loop {
            match self.events.recv().await.unwrap() {
                Event::Disconnected(_) => disconnected = true,
                Event::Closed(_, applied) => {
                    assert!(disconnected);
                    assert!(
                        self.dropped.load(Ordering::Acquire),
                        "transport outlived release receipt"
                    );
                    assert!(
                        !self.task.is_finished(),
                        "supervisor did not wait for the kernel receipt"
                    );
                    applied.send(()).unwrap();
                    break;
                }
                Event::Writable(_) => {}
                _ => panic!("unexpected lifecycle event"),
            }
        }
        (&mut self.task).await.unwrap()
    }
}

#[tokio::test(start_paused = true)]
async fn blocked_write_does_not_block_inbound_and_abort_joins_both_halves() {
    let (mut host, port) = Harness::new().await;
    let slots = Arc::clone(&port.control_slots);
    port.control(vec![7; BLOCKED_FRAME_BYTES]).unwrap();
    host.blocked.notified().await;
    assert_eq!(slots.available_permits(), CONTROL_QUEUE_FRAMES - 1);
    port.control(vec![8; BLOCKED_FRAME_BYTES]).unwrap();
    tokio::time::advance(SOCKET_TIMEOUT * 2).await;
    assert!(
        !host.task.is_finished(),
        "socket backpressure cannot replace kernel liveness policy"
    );
    host.client
        .send(Message::Binary(b"WS2H-inbound".to_vec().into()))
        .await
        .unwrap();
    let event = tokio::time::timeout(SOCKET_TIMEOUT, host.events.recv())
        .await
        .unwrap()
        .unwrap();
    assert!(matches!(&event, Event::Control(_, bytes) if bytes == b"WS2H-inbound"));
    drop(event);
    assert!(!host.dropped.load(Ordering::Acquire));
    drop(port);
    host.released().await.unwrap();
    assert_eq!(slots.available_permits(), CONTROL_QUEUE_FRAMES);
}

#[tokio::test]
async fn unsupported_ingress_and_terminal_reply_drain_before_release() {
    let (mut host, port) = Harness::new().await;
    host.client
        .send(Message::Binary(b"WS2J".to_vec().into()))
        .await
        .unwrap();
    assert!(matches!(
        host.events.recv().await,
        Some(Event::Unsupported(_))
    ));
    let bytes = windshare_relay_core::error_frame::ErrorFrame::UnsupportedMode
        .encode()
        .to_vec();
    port.control(bytes.clone()).unwrap();
    port.finish();
    assert_eq!(
        host.client.next().await.unwrap().unwrap(),
        Message::Binary(bytes.into())
    );
    assert!(!host.dropped.load(Ordering::Acquire));
    assert!(matches!(
        host.client.next().await.unwrap().unwrap(),
        Message::Close(_)
    ));
    assert!(
        !host.dropped.load(Ordering::Acquire),
        "TCP must survive until the close reply"
    );
    host.client.flush().await.unwrap();
    host.released().await.unwrap();
}

#[tokio::test]
async fn server_shutdown_interrupts_a_blocked_writer() {
    let (mut host, port) = Harness::new().await;
    port.control(vec![1; BLOCKED_FRAME_BYTES]).unwrap();
    host.blocked.notified().await;
    host.shutdown.send_replace(true);
    // The actor normally drops the port on Disconnected; an extra producer must
    // not keep the transport tasks or their mailboxes alive during shutdown.
    host.released().await.unwrap();
    assert!(port.control(vec![1]).is_err());
}

#[tokio::test(start_paused = true)]
async fn terminal_drain_has_one_deadline_and_releases_pending_messages() {
    let (mut host, port) = Harness::new().await;
    let slots = Arc::clone(&port.control_slots);
    port.control(vec![1; BLOCKED_FRAME_BYTES]).unwrap();
    host.blocked.notified().await;
    port.control(vec![2; BLOCKED_FRAME_BYTES]).unwrap();
    let started = tokio::time::Instant::now();
    port.finish();
    host.released().await.unwrap();
    assert_eq!(started.elapsed(), SOCKET_TIMEOUT);
    assert_eq!(slots.available_permits(), CONTROL_QUEUE_FRAMES);
}

#[tokio::test]
async fn websocket_ping_flushes_without_an_outbound_application_message() {
    let (mut host, port) = Harness::new().await;
    host.client
        .send(Message::Ping(vec![9].into()))
        .await
        .unwrap();
    assert_eq!(
        host.client.next().await.unwrap().unwrap(),
        Message::Pong(vec![9].into())
    );
    drop(port);
    host.released().await.unwrap();
}

#[tokio::test]
async fn newest_probe_precedes_queued_ordinary_output() {
    let (mut host, port) = Harness::new().await;
    port.control(b"ordered".to_vec()).unwrap();
    port.probe(b"probe-1".to_vec()).unwrap();
    port.probe(b"probe-2".to_vec()).unwrap();
    assert_eq!(
        host.client.next().await.unwrap().unwrap(),
        Message::Binary(b"probe-2".to_vec().into())
    );
    assert_eq!(
        host.client.next().await.unwrap().unwrap(),
        Message::Binary(b"ordered".to_vec().into())
    );
    drop(port);
    host.released().await.unwrap();
}

#[tokio::test]
async fn invalid_input_cancels_writer_and_releases_transport() {
    let (mut host, port) = Harness::new().await;
    host.client
        .send(Message::Text("invalid".into()))
        .await
        .unwrap();
    assert!(host.released().await.is_err());
    assert!(port.control(vec![1]).is_err());
}
