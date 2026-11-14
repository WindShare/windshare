use super::*;
use crate::Model;
use std::{
    net::TcpListener,
    sync::{Arc, Mutex, atomic::AtomicBool, mpsc},
    thread,
};

fn shared() -> Arc<Shared> {
    Arc::new(Shared {
        model: Mutex::new(Model {
            session_id: Some("fixture".into()),
            ..Model::default()
        }),
        closing: AtomicBool::new(false),
        updates: async_channel::bounded(1).0,
        wake: polling::Poller::new().unwrap(),
    })
}

fn listener() -> (TcpListener, Endpoint) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let endpoint = Endpoint {
        address: listener.local_addr().unwrap().to_string(),
        session_id: "fixture".into(),
        token: "fixture-token".into(),
        log_directory: String::new(),
    };
    (listener, endpoint)
}

fn accept(listener: &TcpListener) -> WebSocket<TcpStream> {
    let (stream, _) = listener.accept().unwrap();
    stream.set_read_timeout(Some(IO_TIMEOUT)).unwrap();
    stream.set_write_timeout(Some(IO_TIMEOUT)).unwrap();
    tungstenite::accept(stream).unwrap()
}

fn read_request(socket: &mut WebSocket<TcpStream>) -> serde_json::Value {
    serde_json::from_str(socket.read().unwrap().into_text().unwrap().as_str()).unwrap()
}

#[test]
fn control_reconnect_replays_the_same_request_and_reconciles_full_snapshot() {
    let (listener, endpoint) = listener();
    let shared = shared();
    let server_shared = shared.clone();
    let (commands, receiver) = mpsc::sync_channel(1);
    let server = thread::spawn(move || {
        let mut socket = accept(&listener);
        // Queue after the handshake so the wake also covers a worker that has
        // already drained its outbox and begun waiting for socket readiness.
        commands
            .send(Command::Create {
                operation_id: "selection".into(),
                paths: vec!["file.txt".into()],
            })
            .unwrap();
        server_shared.wake.notify().unwrap();
        let first = read_request(&mut socket);
        drop(socket);
        let mut socket = accept(&listener);
        let replay = read_request(&mut socket);
        assert_eq!(first, replay);
        socket.send(Message::Text(serde_json::json!({
            "version": 1, "id": replay["id"],
            "snapshot": {
                "session_id": "fixture", "revision": 2,
                "shares": [{
                    "operation_id":"selection", "paths":["file.txt"],
                    "state":"awaiting_network", "startup":"pending", "created_at_ms":123,
                    "blocking": {"reason":"directory_unavailable","recovery":"retry","retry_at_ms":456}
                }]
            }
        }).to_string().into())).unwrap();
        socket.close(None).unwrap();
    });
    let mut pending = BTreeMap::new();
    let mut sequence = 0;
    assert!(connection_loop(&endpoint, &shared, &receiver, &mut pending, &mut sequence).is_err());
    assert_eq!(pending.len(), 1);
    assert!(connection_loop(&endpoint, &shared, &receiver, &mut pending, &mut sequence).is_err());
    server.join().unwrap();
    assert!(pending.is_empty());
    assert_eq!(sequence, 1);
    let model = shared.model.lock().unwrap();
    assert_eq!(model.revision, 2);
    assert_eq!(model.operations.len(), 1);
    assert_eq!(model.operations[0].id, "selection");
}

#[test]
fn shutdown_wakes_an_idle_control_socket_without_peer_traffic() {
    let (listener, endpoint) = listener();
    let shared = shared();
    let server_shared = shared.clone();
    let (_commands, receiver) = mpsc::sync_channel(1);
    let (finished, completed) = mpsc::sync_channel(1);
    let server = thread::spawn(move || {
        let _socket = accept(&listener);
        server_shared.closing.store(true, Ordering::Release);
        server_shared.wake.notify().unwrap();
        // Keep the peer alive: socket closure must not be the wakeup source.
        completed.recv_timeout(IO_TIMEOUT).unwrap();
    });
    connection_loop(&endpoint, &shared, &receiver, &mut BTreeMap::new(), &mut 0).unwrap();
    finished.send(()).unwrap();
    server.join().unwrap();
}
