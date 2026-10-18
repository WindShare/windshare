use super::*;
use crate::{
    connection,
    test_support::{Fixture, connection as reference},
};
use futures_util::SinkExt;
use std::{
    io,
    pin::Pin,
    task::{Context, Poll},
    time::Duration,
};
use tokio::{
    io::{AsyncRead, AsyncWrite, DuplexStream, ReadBuf},
    sync::{Notify, oneshot},
    time::timeout,
};
use tokio_tungstenite::{
    WebSocketStream,
    tungstenite::{
        Message,
        protocol::{Role, WebSocketConfig},
    },
};

const TEST_BUDGET: Duration = Duration::from_secs(3);
const PIPE_BYTES: usize = 16;

#[path = "registry_http_tests.rs"]
mod http;

struct HeldTransport {
    stream: DuplexStream,
    blocked: Arc<Notify>,
    dropping: Option<oneshot::Sender<()>>,
    release: std::sync::mpsc::Receiver<()>,
}

impl Drop for HeldTransport {
    fn drop(&mut self) {
        let _ = self.dropping.take().unwrap().send(());
        // Model a writer whose cancellation has begun but whose transport is
        // still alive. No scheduling sleep can masquerade as physical release.
        let _ = self.release.recv_timeout(TEST_BUDGET);
    }
}

impl AsyncRead for HeldTransport {
    fn poll_read(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buf: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        Pin::new(&mut self.stream).poll_read(cx, buf)
    }
}

impl AsyncWrite for HeldTransport {
    fn poll_write(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        bytes: &[u8],
    ) -> Poll<io::Result<usize>> {
        let result = Pin::new(&mut self.stream).poll_write(cx, bytes);
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

fn registry() -> (tempfile::TempDir, Registry, Fixture) {
    let fixture = Fixture::new();
    let directory = tempfile::tempdir().unwrap();
    let mut endpoint = Endpoint::parse("ws://localhost:8787/v2/ws").unwrap();
    endpoint.identity = fixture.identity().relay;
    let store = Database::open(
        &directory.path().join("authority.sqlite"),
        endpoint.identity,
    )
    .unwrap();
    let registry = Registry {
        actors: Mutex::new(BTreeMap::new()),
        store,
        endpoint,
        environment: NativeEnvironment::new().unwrap(),
    };
    (directory, registry, fixture)
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn rebuild_waits_for_writer_destruction_and_fences_old_callbacks() {
    let (_directory, registry, fixture) = registry();
    let share = fixture.identity().share;
    let old = registry.attach(share).unwrap();
    let (server, client) = tokio::io::duplex(PIPE_BYTES);
    let blocked = Arc::new(Notify::new());
    let (dropping, started) = oneshot::channel();
    let (release, released) = std::sync::mpsc::channel();
    let server = WebSocketStream::from_raw_socket(
        HeldTransport {
            stream: server,
            blocked: Arc::clone(&blocked),
            dropping: Some(dropping),
            release: released,
        },
        Role::Server,
        Some(WebSocketConfig::default().write_buffer_size(0)),
    )
    .await;
    let mut client = WebSocketStream::from_raw_socket(client, Role::Client, None).await;
    let (_shutdown, stopping) = watch::channel(false);
    let connection = reference(1);
    let socket = tokio::spawn(
        old.generation
            .admit(
                &old.tx,
                connection::run(
                    server,
                    old.tx.clone(),
                    connection,
                    stopping,
                    old.generation.clone(),
                    crate::test_support::socket_admission(),
                ),
            )
            .unwrap(),
    );
    client
        .send(Message::Binary(fixture.bytes("stopInitB64").into()))
        .await
        .unwrap();
    timeout(TEST_BUDGET, blocked.notified()).await.unwrap();

    old.task.abort();
    timeout(TEST_BUDGET, old.tx.closed()).await.unwrap();
    timeout(TEST_BUDGET, started).await.unwrap().unwrap();
    assert!(!socket.is_finished());
    assert!(
        registry.attach(share).is_err(),
        "closed mailbox bypassed live writer"
    );
    let sibling = ShareId::new([2; 12]).unwrap();
    let _sibling = registry.attach(sibling).unwrap();
    assert_eq!(registry.actors.lock().unwrap().len(), 2);
    assert!(
        registry.attach(share).is_err(),
        "another share's eviction sweep lost the barrier"
    );
    assert!(old.generation.admit(&old.tx, async {}).is_err());

    release.send(()).unwrap();
    timeout(TEST_BUDGET, socket)
        .await
        .unwrap()
        .unwrap()
        .unwrap();
    timeout(TEST_BUDGET, old.generation.wait_released())
        .await
        .unwrap();
    let next = registry.attach(share).unwrap();
    assert_ne!(
        next.generation.identity().incarnation,
        old.generation.identity().incarnation
    );

    // Deliberately reuse the numeric reference: mailbox ownership must fence the
    // old callback independently of the production connection-ID allocator.
    let (port, mut writer, _) = connection::channel(connection);
    next.tx
        .send(Event::Connected(connection, port))
        .await
        .unwrap();
    let (receipt, applied) = oneshot::channel();
    assert!(
        old.tx
            .send(Event::Closed(connection, receipt))
            .await
            .is_err()
    );
    assert!(applied.await.is_err());
    assert!(old.tx.send(Event::Writable(connection)).await.is_err());
    next.tx
        .send(Event::Control(connection, fixture.bytes("stopInitB64")))
        .await
        .unwrap();
    assert_eq!(
        &timeout(TEST_BUDGET, writer.recv()).await.unwrap().unwrap()[..4],
        b"WS2Q"
    );
    registry.shutdown().await.unwrap();
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn cancelled_connection_supervisor_keeps_host_lease_until_children_exit() {
    let (directory, registry, fixture) = registry();
    let old = registry.attach(fixture.identity().share).unwrap();
    let (server, client) = tokio::io::duplex(PIPE_BYTES);
    let blocked = Arc::new(Notify::new());
    let (dropping, started) = oneshot::channel();
    let (release, released) = std::sync::mpsc::channel();
    let server = WebSocketStream::from_raw_socket(
        HeldTransport {
            stream: server,
            blocked: Arc::clone(&blocked),
            dropping: Some(dropping),
            release: released,
        },
        Role::Server,
        Some(WebSocketConfig::default().write_buffer_size(0)),
    )
    .await;
    let mut client = WebSocketStream::from_raw_socket(client, Role::Client, None).await;
    let (_shutdown, stopping) = watch::channel(false);
    let socket = tokio::spawn(
        old.generation
            .admit(
                &old.tx,
                connection::run(
                    server,
                    old.tx.clone(),
                    reference(1),
                    stopping,
                    old.generation.clone(),
                    crate::test_support::socket_admission(),
                ),
            )
            .unwrap(),
    );
    client
        .send(Message::Binary(fixture.bytes("stopInitB64").into()))
        .await
        .unwrap();
    timeout(TEST_BUDGET, blocked.notified()).await.unwrap();
    socket.abort();
    timeout(TEST_BUDGET, started).await.unwrap().unwrap();
    assert!(
        timeout(TEST_BUDGET, socket)
            .await
            .unwrap()
            .unwrap_err()
            .is_cancelled()
    );
    let actor = old.task.clone();
    drop(old);
    drop(registry);
    actor.abort();
    let path = directory.path().join("authority.sqlite");
    assert!(
        Database::open(&path, fixture.identity().relay).is_err(),
        "host lease outlived by a transport child"
    );
    release.send(()).unwrap();
    // Reopening is itself the host-boundary proof, after every tracked descendant
    // and the independent cleanup monitor have relinquished the authority lease.
    let reopened = timeout(TEST_BUDGET, async {
        loop {
            if let Ok(store) = Database::open(&path, fixture.identity().relay) {
                break store;
            }
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
    assert!(reopened.generation() > NonZeroU64::MIN);
}
