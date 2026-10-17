//! Opt-in native host experiment: real WebSocket writers, canonical authenticated core.
mod scenario;

use futures_util::{SinkExt, StreamExt};
use scenario::{FRAME_BYTES, Scenario, WINDOW, payload};
use serde_json::{Value, json};
use socket2::SockRef;
use std::{
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::{Duration, Instant},
};
use tokio::{
    net::{TcpListener, TcpStream},
    sync::{Semaphore, mpsc},
    task::JoinHandle,
};
use tokio_tungstenite::{
    WebSocketStream, accept_async_with_config, client_async,
    tungstenite::{Message, protocol::WebSocketConfig},
};
use windshare_relay_core::{
    flow::{Counts, DeliveryConsumed, FlowKey},
    share::{Input, Submission},
};

const HEALTHY_FRAMES: usize = 512;
const FEEDBACK_BATCH: usize = 4;
const WRITER_QUEUE: usize = 2;
const SMALL_SOCKET_BUFFER: usize = 8 * 1024;
const RUN_TIMEOUT: Duration = Duration::from_secs(20);

type Relay = Arc<Mutex<Scenario>>;
type Socket = WebSocketStream<TcpStream>;

#[derive(Default)]
struct Samples {
    lock_wait_ns: Vec<u64>,
    critical_ns: Vec<u64>,
    submit_ns: Vec<u64>,
}
type Metrics = Arc<Mutex<Samples>>;

struct Connection {
    id: u64,
    flow: FlowKey,
    sender: mpsc::Sender<Message>,
    producer: JoinHandle<()>,
    reader: JoinHandle<()>,
    writer: JoinHandle<()>,
    client: Option<JoinHandle<Socket>>,
    paused_client: Option<Socket>,
    writes_completed: Arc<AtomicUsize>,
    submitted: Arc<AtomicUsize>,
    consumed: Arc<AtomicUsize>,
}

async fn sockets(slow: bool) -> (Socket, Socket) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let server = async {
        let (stream, _) = listener.accept().await.unwrap();
        stream.set_nodelay(true).unwrap();
        // Closed evidence requires releasing TCP buffers independently of STOP.
        // Default TCP close can keep sending after application handles are dropped.
        SockRef::from(&stream)
            .set_linger(Some(Duration::ZERO))
            .unwrap();
        if slow {
            SockRef::from(&stream)
                .set_send_buffer_size(SMALL_SOCKET_BUFFER)
                .unwrap();
        }
        accept_async_with_config(
            stream,
            Some(
                WebSocketConfig::default()
                    .write_buffer_size(0)
                    .max_write_buffer_size(FRAME_BYTES * 2)
                    .max_message_size(Some(FRAME_BYTES)),
            ),
        )
        .await
        .unwrap()
    };
    let client = async {
        let stream = TcpStream::connect(address).await.unwrap();
        stream.set_nodelay(true).unwrap();
        if slow {
            SockRef::from(&stream)
                .set_recv_buffer_size(SMALL_SOCKET_BUFFER)
                .unwrap();
        }
        client_async(format!("ws://{address}/probe"), stream)
            .await
            .unwrap()
            .0
    };
    tokio::join!(server, client)
}

async fn healthy_client(mut socket: Socket, flow: FlowKey) -> Socket {
    let mut received = 0;
    while received < HEALTHY_FRAMES {
        let message = socket
            .next()
            .await
            .expect("live socket")
            .expect("valid frame");
        let Message::Binary(bytes) = message else {
            continue;
        };
        assert_eq!(bytes.len(), FRAME_BYTES);
        assert_eq!(&bytes[..8], b"WS2O\x02\0\0\0");
        assert_eq!(&bytes[8..16], flow.session().wire_id().as_bytes());
        assert_eq!(
            u64::from_be_bytes(bytes[20..28].try_into().unwrap()),
            received as u64
        );
        assert!(bytes[28..].iter().all(|byte| *byte == 0x5a));
        received += 1;
        if received % FEEDBACK_BATCH == 0 {
            let feedback = DeliveryConsumed::new(
                flow.session().wire_id(),
                Counts::new(received as u64, (received * FRAME_BYTES) as u64),
            )
            .unwrap();
            socket
                .send(Message::Binary(feedback.encode().to_vec().into()))
                .await
                .unwrap();
        }
    }
    socket
}

async fn connect(id: u64, slow: bool, relay: Relay, metrics: Metrics) -> Connection {
    let flow = relay.lock().unwrap().join(id);
    let (server, client) = sockets(slow).await;
    let (mut sink, mut source) = server.split();
    let (sender, mut pending) = mpsc::channel::<Message>(WRITER_QUEUE);
    let writes_completed = Arc::new(AtomicUsize::new(0));
    let written = writes_completed.clone();
    let writer = tokio::spawn(async move {
        while let Some(message) = pending.recv().await {
            if sink.send(message).await.is_err() {
                break;
            }
            written.fetch_add(1, Ordering::Relaxed);
        }
    });
    let credits = Arc::new(Semaphore::new(WINDOW));
    let replenish = credits.clone();
    let receiver_relay = relay.clone();
    let consumed_count = Arc::new(AtomicUsize::new(0));
    let observed_consumption = consumed_count.clone();
    let reader = tokio::spawn(async move {
        let mut consumed = Counts::ZERO;
        while let Some(Ok(Message::Binary(bytes))) = source.next().await {
            let feedback = DeliveryConsumed::decode(&bytes).expect("consumption feedback");
            assert_eq!(feedback.session(), flow.session().wire_id());
            let total = feedback.total();
            let delta = Counts::new(
                total.frames - consumed.frames,
                total.encoded_bytes - consumed.encoded_bytes,
            );
            receiver_relay.lock().unwrap().consume(flow, total, delta);
            replenish.add_permits(delta.frames as usize);
            consumed = total;
            observed_consumption.store(total.frames as usize, Ordering::Release);
        }
    });
    let submitted = Arc::new(AtomicUsize::new(0));
    let charged = submitted.clone();
    let queue = sender.clone();
    let producer = tokio::spawn(async move {
        let frames = if slow { WINDOW } else { HEALTHY_FRAMES };
        let mut local = Samples::default();
        for ordinal in 0..frames {
            credits.acquire().await.unwrap().forget();
            let bytes = payload(flow, ordinal as u64);
            // Reserve without the share lock. Each destination can stall independently.
            let permit = queue.reserve().await.unwrap();
            let started = Instant::now();
            let sample = {
                let mut state = relay.lock().unwrap();
                let locked = Instant::now();
                let receipt = state.receive(&bytes);
                let begin_submit = Instant::now();
                state.submit(&receipt, |_| {
                    permit.send(Message::Binary(bytes.into()));
                    Submission::Complete
                });
                let finished = Instant::now();
                (
                    (locked - started).as_nanos() as u64,
                    (finished - locked).as_nanos() as u64,
                    (finished - begin_submit).as_nanos() as u64,
                )
            };
            local.lock_wait_ns.push(sample.0);
            local.critical_ns.push(sample.1);
            local.submit_ns.push(sample.2);
            charged.fetch_add(1, Ordering::Relaxed);
        }
        let mut result = metrics.lock().unwrap();
        result.lock_wait_ns.extend(local.lock_wait_ns);
        result.critical_ns.extend(local.critical_ns);
        result.submit_ns.extend(local.submit_ns);
    });
    let (client, paused_client) = if slow {
        (None, Some(client))
    } else {
        (Some(tokio::spawn(healthy_client(client, flow))), None)
    };
    Connection {
        id,
        flow,
        sender,
        producer,
        reader,
        writer,
        client,
        paused_client,
        writes_completed,
        submitted,
        consumed: consumed_count,
    }
}

fn joined(result: Result<(), tokio::task::JoinError>) {
    if let Err(error) = result {
        assert!(
            error.is_cancelled(),
            "host task failed before cleanup: {error}"
        );
    }
}

fn distribution(values: &mut [u64]) -> Value {
    values.sort_unstable();
    assert!(!values.is_empty());
    let percentile = |percent: usize| values[(values.len() - 1) * percent / 100];
    json!({"samples": values.len(), "p50_ns": percentile(50), "p95_ns": percentile(95),
        "p99_ns": percentile(99), "max_ns": values[values.len() - 1]})
}

async fn run(healthy: usize, with_slow: bool) -> Value {
    let relay = Arc::new(Mutex::new(Scenario::new()));
    let metrics = Arc::new(Mutex::new(Samples::default()));
    let mut connections = Vec::new();
    if with_slow {
        connections.push(connect(2, true, relay.clone(), metrics.clone()).await);
    }
    let started = Instant::now();
    for index in 0..healthy {
        connections.push(connect(10 + index as u64, false, relay.clone(), metrics.clone()).await);
    }
    let mut finished_clients = Vec::new();
    for connection in &mut connections {
        if let Some(client) = connection.client.take() {
            finished_clients.push(client.await.expect("healthy client verified payload"));
        }
    }
    let elapsed = started.elapsed();
    for connection in &connections {
        if connection.id != 2 {
            while connection.consumed.load(Ordering::Acquire) < HEALTHY_FRAMES {
                tokio::task::yield_now().await;
            }
        }
    }
    let slow = connections.iter().find(|connection| connection.id == 2);
    if let Some(slow) = slow {
        // Observe actual writer backpressure, not an artificial sleep inside the host callback.
        let deadline = Instant::now() + Duration::from_secs(2);
        while slow.sender.capacity() != 0 && Instant::now() < deadline {
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert_eq!(
            slow.sender.capacity(),
            0,
            "slow socket must back up its bounded writer"
        );
        assert!(
            slow.submitted.load(Ordering::Relaxed) < WINDOW,
            "the slow producer must still be waiting outside the share lock"
        );
    }
    let slow_state = slow.map(|connection| {
        json!({
            "submitted": connection.submitted.load(Ordering::Relaxed),
            "writes_completed": connection.writes_completed.load(Ordering::Relaxed),
            "writer_queue_used": WRITER_QUEUE - connection.sender.capacity(),
            "configured_socket_buffer_bytes": SMALL_SOCKET_BUFFER,
        })
    });
    let flow = connections
        .iter()
        .find(|connection| connection.id != 2)
        .unwrap()
        .flow;
    let pending_bytes = payload(flow, HEALTHY_FRAMES as u64);
    let pending_receipt = relay.lock().unwrap().receive(&pending_bytes);
    let begin_stop = Instant::now();
    let stop_lock_wait;
    {
        let mut state = relay.lock().unwrap();
        stop_lock_wait = begin_stop.elapsed();
        state.stop();
        assert!(
            !state.stop_acknowledged,
            "no STOP ack before unsubmitted queues are discarded"
        );
        let mut handed_off = false;
        let now = state.now;
        let refused = state.relay.submit(&pending_receipt, now, |_| {
            handed_off = true;
            Submission::Complete
        });
        assert!(refused.refusal.is_some());
        assert!(
            !handed_off,
            "a receipt acquired before STOP cannot authorize a later handoff"
        );
    }
    let revoke_elapsed = begin_stop.elapsed();
    drop(pending_bytes);
    // Join cancellation before reporting Closed. Both split halves and the writer queue
    // must be dropped; abortive TCP close also discards the OS send queue.
    for connection in &connections {
        connection.producer.abort();
    }
    for connection in &mut connections {
        joined((&mut connection.producer).await);
    }
    {
        let mut state = relay.lock().unwrap();
        for flow in std::mem::take(&mut state.discard_requests) {
            state.handle(Input::Discarded(flow));
        }
        assert!(
            state.stop_acknowledged,
            "STOP must not wait for transport release"
        );
    }
    let cleanup_started = Instant::now();
    let mut paused_client = None;
    for connection in connections {
        connection.writer.abort();
        connection.reader.abort();
        joined(connection.writer.await);
        joined(connection.reader.await);
        drop(connection.sender);
        if connection.paused_client.is_some() {
            paused_client = connection.paused_client;
        }
        relay.lock().unwrap().closed(connection.id);
    }
    let cleanup_elapsed = cleanup_started.elapsed();
    let trace_count;
    {
        let mut state = relay.lock().unwrap();
        // The fixture sender never owns a socket; only receiver adapters own transport resources.
        state.closed(1);
        assert!(state.stop_acknowledged);
        trace_count = state.relay.observer().events.len();
    }
    // Keep the paused peer alive until server teardown has independently terminated it.
    let slow_peer_terminal = if let Some(mut socket) = paused_client {
        loop {
            match socket.next().await {
                Some(Ok(Message::Binary(_))) => continue,
                Some(Err(error)) => break Some(error.to_string()),
                None => break Some("stream ended without a close handshake".to_owned()),
                other => panic!("expected abortive peer termination: {other:?}"),
            }
        }
    } else {
        None
    };
    drop(finished_clients);
    let mut samples = metrics.lock().unwrap();
    json!({"healthy_receivers": healthy, "slow_receiver": with_slow,
        "healthy_frames_per_receiver": HEALTHY_FRAMES, "encoded_frame_bytes": FRAME_BYTES,
        "healthy_mib": (healthy * HEALTHY_FRAMES * FRAME_BYTES) as f64 / 1_048_576.0,
        "elapsed_ms": elapsed.as_secs_f64() * 1000.0,
        "healthy_mib_per_s": (healthy * HEALTHY_FRAMES * FRAME_BYTES) as f64 / 1_048_576.0 / elapsed.as_secs_f64(),
        "lock_wait": distribution(&mut samples.lock_wait_ns),
        "receive_and_submit_critical_section": distribution(&mut samples.critical_ns),
        "submit": distribution(&mut samples.submit_ns),
        "stop_lock_wait_ns": stop_lock_wait.as_nanos() as u64,
        "stop_revoke_ns": revoke_elapsed.as_nanos() as u64,
        "joined_writer_cleanup_ns": cleanup_elapsed.as_nanos() as u64,
        "stale_submission_refused": true, "stop_ack_after_queue_discard": true,
        "stop_ack_before_transport_release": true,
        "slow_state": slow_state, "slow_peer_terminal": slow_peer_terminal,
        "close_mode": "SO_LINGER=0 after joining both WebSocket halves",
        "milestone_observations": trace_count})
}

#[tokio::main(flavor = "multi_thread", worker_threads = 4)]
async fn main() {
    let mut runs = Vec::new();
    for (healthy, slow) in [(1, false), (1, true), (4, true)] {
        runs.push(
            tokio::time::timeout(RUN_TIMEOUT, run(healthy, slow))
                .await
                .expect("bounded experiment"),
        );
    }
    println!("{}", serde_json::to_string_pretty(&json!({
        "platform": std::env::consts::OS, "architecture": std::env::consts::ARCH,
        "profile": if cfg!(debug_assertions) { "debug" } else { "release" },
        "transport": "loopback TCP WebSocket", "worker_threads": 4,
        "client_feedback_batch": FEEDBACK_BATCH, "writer_queue_frames": WRITER_QUEUE,
        "limitations": "Canonical fixture handshake and in-memory storage; not product throughput or durability certification.",
        "runs": runs
    })).unwrap());
}
