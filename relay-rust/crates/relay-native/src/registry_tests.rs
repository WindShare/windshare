use super::*;
use crate::connection::AdmissionPriority::Authenticated;
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
use windshare_relay_core::clock::UnixMillis;

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

struct HeldSocket {
    server: WebSocketStream<HeldTransport>,
    client: WebSocketStream<DuplexStream>,
    blocked: Arc<Notify>,
    started: oneshot::Receiver<()>,
    release: std::sync::mpsc::Sender<()>,
}

async fn held_socket() -> HeldSocket {
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
    let client = WebSocketStream::from_raw_socket(client, Role::Client, None).await;
    HeldSocket {
        server,
        client,
        blocked,
        started,
        release,
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
        node: crate::node::Runtime::default(),
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
    let old = registry.attach(share, Authenticated).unwrap();
    let HeldSocket {
        server,
        mut client,
        blocked,
        started,
        release,
    } = held_socket().await;
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
                    crate::node::Runtime::default(),
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
        registry.attach(share, Authenticated).is_err(),
        "closed mailbox bypassed live writer"
    );
    let sibling = ShareId::new([2; 12]).unwrap();
    let _sibling = registry.attach(sibling, Authenticated).unwrap();
    assert_eq!(registry.actors.lock().unwrap().len(), 2);
    assert!(
        registry.attach(share, Authenticated).is_err(),
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
    let next = registry.attach(share, Authenticated).unwrap();
    assert_ne!(
        next.generation.identity().incarnation,
        old.generation.identity().incarnation
    );

    // Deliberately reuse the numeric reference: mailbox ownership must fence the
    // old callback independently of the production connection-ID allocator.
    let (port, mut writer, _) = connection::channel(connection, crate::node::Runtime::default());
    next.tx
        .send(Event::Connected(connection, Box::new(port)))
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

fn two_source_capacity(now: UnixMillis) -> Arc<Capacity> {
    Capacity::new(
        ConnectionLimits {
            controls: 2,
            controls_per_source: 1,
            ..ConnectionLimits::default()
        },
        CONNECTION_ATTEMPTS,
        now,
    )
    .unwrap()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn cancelled_connection_supervisor_keeps_host_lease_until_children_exit() {
    let (directory, registry, fixture) = registry();
    let now = registry.environment.now();
    let capacity = two_source_capacity(now);
    let source = "192.0.2.1".parse().unwrap();
    let admission = capacity
        .accept(source, reference(1), now, Authenticated)
        .unwrap();
    let old = registry
        .attach(fixture.identity().share, Authenticated)
        .unwrap();
    let HeldSocket {
        server,
        mut client,
        blocked,
        started,
        release,
    } = held_socket().await;
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
                    admission,
                    crate::node::Runtime::default(),
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
    assert!(
        capacity
            .accept(source, reference(2), now, Authenticated)
            .is_err()
    );
    let other = capacity
        .accept(
            "192.0.2.2".parse().unwrap(),
            reference(3),
            registry.environment.now(),
            Authenticated,
        )
        .unwrap();
    assert!(
        capacity
            .accept(
                "192.0.2.3".parse().unwrap(),
                reference(4),
                now,
                Authenticated
            )
            .is_err()
    );
    drop(other);
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
    assert!(
        capacity
            .accept(source, reference(5), now, Authenticated)
            .is_ok()
    );
}

struct SignedShare {
    signing_key: ed25519_dalek::SigningKey,
    share_id: ShareId,
    pk_hash: [u8; 16],
    share_instance: [u8; 16],
    descriptor: Vec<u8>,
    descriptor_digest: [u8; 32],
    resume_token_hash: [u8; 32],
    receive_capacity: [u8; 16],
    relay: windshare_relay_core::identifiers::RelayIdentity,
}

impl SignedShare {
    fn new(seed: u8, relay: windshare_relay_core::identifiers::RelayIdentity) -> Self {
        use sha2::{Digest as _, Sha256};

        let signing_key = ed25519_dalek::SigningKey::from_bytes(&[seed; 32]);
        let public = signing_key.verifying_key().to_bytes();
        let pk_hash: [u8; 16] = Sha256::new()
            .chain_update(b"windshare/v2 sender-key\0")
            .chain_update(public)
            .finalize()[..16]
            .try_into()
            .unwrap();
        let share_id_bytes: [u8; 12] = Sha256::new()
            .chain_update(b"windshare/v2 share-id\0")
            .chain_update(pk_hash)
            .finalize()[..12]
            .try_into()
            .unwrap();
        let descriptor = vec![seed; 32];
        let descriptor_digest: [u8; 32] = Sha256::digest(&descriptor).into();
        let mut receive_capacity = [0u8; 16];
        receive_capacity[..8].copy_from_slice(&16u64.to_be_bytes());
        receive_capacity[8..].copy_from_slice(&(64u64 * 1024).to_be_bytes());
        Self {
            signing_key,
            share_id: ShareId::new(share_id_bytes).unwrap(),
            pk_hash,
            share_instance: [seed; 16],
            descriptor,
            descriptor_digest,
            resume_token_hash: [seed; 32],
            receive_capacity,
            relay,
        }
    }

    fn register_init(&self) -> Vec<u8> {
        let mut frame = Vec::with_capacity(132);
        frame.extend_from_slice(b"WS2R");
        frame.extend_from_slice(&[2, 0, 0, 0]);
        frame.extend_from_slice(self.share_id.as_bytes());
        frame.extend_from_slice(&self.share_instance);
        frame.extend_from_slice(&self.pk_hash);
        frame.extend_from_slice(&self.descriptor_digest);
        frame.extend_from_slice(&self.resume_token_hash);
        frame.extend_from_slice(&self.receive_capacity);
        frame
    }

    fn register_proof(&self, challenge: &[u8]) -> Vec<u8> {
        use ed25519_dalek::Signer as _;

        let mut preimage = Vec::new();
        preimage.extend_from_slice(b"windshare/v2 relay-register\0");
        preimage.extend_from_slice(self.share_id.as_bytes());
        preimage.extend_from_slice(&self.share_instance);
        preimage.extend_from_slice(&self.pk_hash);
        preimage.extend_from_slice(&self.descriptor_digest);
        preimage.extend_from_slice(&self.resume_token_hash);
        preimage.extend_from_slice(self.relay.as_bytes());
        preimage.extend_from_slice(&challenge[8..64]);
        preimage.extend_from_slice(&self.receive_capacity);
        let signature = self.signing_key.sign(&preimage);
        let mut frame = Vec::with_capacity(104);
        frame.extend_from_slice(b"WS2P");
        frame.extend_from_slice(&[2, 0, 0, 0]);
        frame.extend_from_slice(&self.signing_key.verifying_key().to_bytes());
        frame.extend_from_slice(&signature.to_bytes());
        frame
    }

    fn descriptor_upload(&self) -> Vec<u8> {
        let mut frame = Vec::with_capacity(12 + self.descriptor.len());
        frame.extend_from_slice(b"WS2U");
        frame.extend_from_slice(&[2, 0, 0, 0]);
        frame.extend_from_slice(&u32::try_from(self.descriptor.len()).unwrap().to_be_bytes());
        frame.extend_from_slice(&self.descriptor);
        frame
    }

    fn stop_init(&self, stop_id: [u8; 16]) -> Vec<u8> {
        let mut frame = Vec::with_capacity(100);
        frame.extend_from_slice(b"WS2X");
        frame.extend_from_slice(&[2, 0, 0, 0]);
        frame.extend_from_slice(self.share_id.as_bytes());
        frame.extend_from_slice(&self.share_instance);
        frame.extend_from_slice(&self.pk_hash);
        frame.extend_from_slice(self.relay.as_bytes());
        frame.extend_from_slice(&stop_id);
        frame
    }

    fn stop_proof(&self, stop_id: [u8; 16], challenge: &[u8]) -> Vec<u8> {
        use ed25519_dalek::Signer as _;

        let mut preimage = Vec::new();
        preimage.extend_from_slice(b"windshare/v2 relay-stop\0");
        preimage.extend_from_slice(self.share_id.as_bytes());
        preimage.extend_from_slice(&self.share_instance);
        preimage.extend_from_slice(&self.pk_hash);
        preimage.extend_from_slice(self.relay.as_bytes());
        preimage.extend_from_slice(&stop_id);
        preimage.extend_from_slice(&challenge[8..64]);
        let signature = self.signing_key.sign(&preimage);
        let mut frame = Vec::with_capacity(104);
        frame.extend_from_slice(b"WS2V");
        frame.extend_from_slice(&[2, 0, 0, 0]);
        frame.extend_from_slice(&self.signing_key.verifying_key().to_bytes());
        frame.extend_from_slice(&signature.to_bytes());
        frame
    }
}

fn registry_with_single_share_quota(path: &Path) -> (Registry, &'static str) {
    use crate::node::access::Policy;
    use windshare_relay_access::{Credential, CredentialState, Digest};

    const SECRET: &str = "single-share-service-key";
    let endpoint = Endpoint::parse("ws://localhost:8787/v2/ws").unwrap();
    let store = Database::open(path, endpoint.identity).unwrap();
    let node = crate::node::Runtime::default();
    node.access
        .configure(Policy {
            anonymous: false,
            shares_per_principal: 1,
            credentials: vec![Credential {
                principal: [7; 16],
                id: [7; 16],
                name: "guest".to_owned(),
                digest: Digest::of(SECRET.as_bytes()),
                created_at: 0,
                last_used_at: None,
                state: CredentialState::Active,
            }],
            ..Policy::default()
        })
        .unwrap();
    let registry = Registry {
        node,
        actors: Mutex::new(BTreeMap::new()),
        store,
        endpoint,
        environment: NativeEnvironment::new().unwrap(),
    };
    (registry, SECRET)
}

async fn connect_share(
    registry: &Registry,
    share: ShareId,
    conn_id: u64,
    secret: Option<&str>,
) -> (Arc<Handle>, connection::Writer) {
    let handle = registry.attach(share, Authenticated).unwrap();
    let admission = crate::test_support::socket_admission();
    if let Some(secret) = secret {
        let mut bearer = crate::node::access::Bearer::new(secret);
        registry.node.access.admission_priority(
            share,
            Some(&mut bearer),
            registry.environment.now().0 / 1000,
        );
        admission.set_bearer(bearer);
    }
    let (port, writer, _) = connection::channel(reference(conn_id), registry.node.clone());
    handle
        .tx
        .send(Event::Connected(
            reference(conn_id),
            Box::new(port.with_admission(&admission)),
        ))
        .await
        .unwrap();
    (handle, writer)
}

async fn read_frame(writer: &mut connection::Writer) -> Vec<u8> {
    timeout(TEST_BUDGET, writer.recv())
        .await
        .unwrap()
        .expect("expected relay control frame")
}

async fn publish_share(
    registry: &Registry,
    share: &SignedShare,
    conn_id: u64,
    secret: &str,
) -> (Arc<Handle>, connection::Writer) {
    let (handle, mut writer) = connect_share(registry, share.share_id, conn_id, Some(secret)).await;
    handle
        .tx
        .send(Event::Control(reference(conn_id), share.register_init()))
        .await
        .unwrap();
    let challenge = read_frame(&mut writer).await;
    assert_eq!(&challenge[..4], b"WS2Q");
    handle
        .tx
        .send(Event::Control(
            reference(conn_id),
            share.register_proof(&challenge),
        ))
        .await
        .unwrap();
    handle
        .tx
        .send(Event::Control(
            reference(conn_id),
            share.descriptor_upload(),
        ))
        .await
        .unwrap();
    let registered = read_frame(&mut writer).await;
    assert_eq!(&registered[..4], b"WS2K");
    (handle, writer)
}

async fn stop_share(
    registry: &Registry,
    share: &SignedShare,
    conn_id: u64,
    stop_id: [u8; 16],
) -> (Arc<Handle>, connection::Writer) {
    let (handle, mut writer) = connect_share(registry, share.share_id, conn_id, None).await;
    handle
        .tx
        .send(Event::Control(reference(conn_id), share.stop_init(stop_id)))
        .await
        .unwrap();
    let challenge = read_frame(&mut writer).await;
    assert_eq!(&challenge[..4], b"WS2Q");
    handle
        .tx
        .send(Event::Control(
            reference(conn_id),
            share.stop_proof(stop_id, &challenge),
        ))
        .await
        .unwrap();
    let stopped = read_frame(&mut writer).await;
    assert_eq!(&stopped[..4], b"WS2Y");
    (handle, writer)
}

#[tokio::test]
async fn stop_releases_principal_residency_for_queued_share_without_unrelated_eviction() {
    use windshare_relay_core::error_frame::ErrorFrame;

    let directory = tempfile::tempdir().unwrap();
    let (registry, secret) =
        registry_with_single_share_quota(&directory.path().join("authority.sqlite"));
    let share_a = SignedShare::new(11, registry.endpoint.identity);
    let share_b = SignedShare::new(22, registry.endpoint.identity);

    let (handle_a, _sender_a) = publish_share(&registry, &share_a, 1, secret).await;

    // Share B queues in `registry.actors` and is refused while share A holds the sole slot.
    let (handle_b, mut queued_b) =
        connect_share(&registry, share_b.share_id, 2, Some(secret)).await;
    handle_b
        .tx
        .send(Event::Control(reference(2), share_b.register_init()))
        .await
        .unwrap();
    let refusal = read_frame(&mut queued_b).await;
    assert_eq!(
        refusal,
        ErrorFrame::AccessUnavailable(
            windshare_relay_core::error_frame::RetryAfter::from_millis(1_000).unwrap()
        )
        .encode()
    );
    drop(queued_b);
    drop(handle_b);

    // Stopping share A must release its residency before STOPPED even while its actor stays resident.
    let (stop_handle, _stopper) = stop_share(&registry, &share_a, 3, [9; 16]).await;
    assert_eq!(registry.actors.lock().unwrap().len(), 2);

    // Retrying share B hits the existing-actor fast path in `Registry::attach` without evicting A.
    let (_published_b, _sender_b) = publish_share(&registry, &share_b, 4, secret).await;
    assert_eq!(registry.actors.lock().unwrap().len(), 2);

    // A stale REGISTER on stopped share A must return Stopped instead of reserving a slot.
    let (stale_a, mut stale_writer) =
        connect_share(&registry, share_a.share_id, 5, Some(secret)).await;
    stale_a
        .tx
        .send(Event::Control(reference(5), share_a.register_init()))
        .await
        .unwrap();
    assert_eq!(
        read_frame(&mut stale_writer).await,
        ErrorFrame::Stopped.encode()
    );

    drop(stale_a);
    drop(stop_handle);
    drop(handle_a);
    registry.shutdown().await.unwrap();
}

#[tokio::test]
async fn stale_register_on_restarted_stopped_share_does_not_leak_residency() {
    use windshare_relay_core::error_frame::ErrorFrame;

    let directory = tempfile::tempdir().unwrap();
    let db_path = directory.path().join("authority.sqlite");
    let (registry, secret) = registry_with_single_share_quota(&db_path);
    let share_a = SignedShare::new(33, registry.endpoint.identity);
    let share_b = SignedShare::new(44, registry.endpoint.identity);

    let (handle_a, _sender_a) = publish_share(&registry, &share_a, 1, secret).await;
    let (stop_handle, _stopper) = stop_share(&registry, &share_a, 2, [8; 16]).await;
    drop(stop_handle);
    drop(handle_a);
    registry.shutdown().await.unwrap();
    drop(registry);

    let (restarted, secret) = registry_with_single_share_quota(&db_path);
    let (stale_a, mut stale_writer) =
        connect_share(&restarted, share_a.share_id, 3, Some(secret)).await;
    stale_a
        .tx
        .send(Event::Control(reference(3), share_a.register_init()))
        .await
        .unwrap();
    assert_eq!(
        read_frame(&mut stale_writer).await,
        ErrorFrame::Stopped.encode()
    );
    drop(stale_writer);

    // Even with stopped share A still held in `actors`, share B can claim the sole slot.
    let (_published_b, _sender_b) = publish_share(&restarted, &share_b, 4, secret).await;
    drop(stale_a);
    restarted.shutdown().await.unwrap();
}
