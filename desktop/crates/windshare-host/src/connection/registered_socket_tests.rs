use super::*;
use std::{
    io::{Read as _, Write as _},
    net::TcpListener,
    panic::{AssertUnwindSafe, catch_unwind, panic_any},
    time::Duration,
};
use tungstenite::protocol::Role;

const STALE_EVENT_WINDOW: Duration = Duration::from_millis(20);
const SOCKET_TIMEOUT: Duration = Duration::from_secs(2);

fn socket_pair() -> (WebSocket<TcpStream>, TcpStream) {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let stream = TcpStream::connect(listener.local_addr().unwrap()).unwrap();
    let (peer, _) = listener.accept().unwrap();
    stream.set_nonblocking(true).unwrap();
    peer.set_read_timeout(Some(SOCKET_TIMEOUT)).unwrap();
    peer.set_write_timeout(Some(SOCKET_TIMEOUT)).unwrap();
    (WebSocket::from_raw_socket(stream, Role::Client, None), peer)
}

#[test]
fn removal_allows_registering_the_same_live_socket_again() {
    let (socket, mut peer) = socket_pair();
    let poller = Poller::new().unwrap();
    let mut registered = RegisteredSocket::new(socket, &poller, "fixture").unwrap();
    let socket = registered.unregister().unwrap().unwrap();
    // Keeping the same handle alive makes missing deregistration observable;
    // closing it first could let the OS silently discard a stale registration.
    let registered_again = RegisteredSocket::new(socket, &poller, "fixture").unwrap();
    registered_again.close().unwrap();
    assert_eq!(peer.read(&mut [0]).unwrap(), 0);
    registered.close().unwrap();
}

#[test]
fn unwinding_removes_interest_even_while_the_connection_stays_alive() {
    let (socket, mut peer) = socket_pair();
    // On epoll, a duplicated descriptor keeps stale interests alive even when
    // the original handle closes. This also avoids peer-close events as noise.
    let retained_connection = socket.get_ref().try_clone().unwrap();
    let poller = Poller::new().unwrap();
    let result = catch_unwind(AssertUnwindSafe(|| {
        let _registered = RegisteredSocket::new(socket, &poller, "unwind-fixture").unwrap();
        poller
            .wait(&mut polling::Events::new(), Some(Duration::ZERO))
            .unwrap();
        panic_any("fixture panic");
    }));
    assert!(result.is_err());
    peer.write_all(b"data after unwind").unwrap();
    let mut events = polling::Events::new();
    poller.wait(&mut events, Some(STALE_EVENT_WINDOW)).unwrap();
    assert!(events.is_empty(), "unwinding retained socket interest");
    drop(retained_connection);
}
