use super::*;
use crate::{Endpoint, OriginPolicy, Server};
use std::time::Duration;
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    time::timeout,
};
use tokio_util::sync::CancellationToken;

const TEST_BUDGET: Duration = Duration::from_secs(3);
const BINDING: &[u8] = &[
    0, 1, 0, 0, 0x21, 0x12, 0xa4, 0x42, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12,
];

fn config(addresses: Vec<SocketAddr>) -> Config {
    Config {
        udp_addresses: addresses,
        admin_address: Some("127.0.0.1:0".parse().unwrap()),
        ..Config::default()
    }
}

async fn http(address: SocketAddr, path: &str) -> String {
    timeout(TEST_BUDGET, async {
        let mut stream = TcpStream::connect(address).await.unwrap();
        stream
            .write_all(format!("GET {path} HTTP/1.1\r\nHost: localhost\r\n\r\n").as_bytes())
            .await
            .unwrap();
        let mut response = String::new();
        stream.read_to_string(&mut response).await.unwrap();
        response
    })
    .await
    .unwrap()
}

async fn binding(address: SocketAddr) {
    let local = if address.is_ipv4() {
        "127.0.0.1:0"
    } else {
        "[::1]:0"
    };
    let socket = UdpSocket::bind(local).await.unwrap();
    // A valid header followed by oversized junk must not be accepted as a
    // truncated Binding, nor may WSAEMSGSIZE retire the socket on Windows.
    let mut oversized = BINDING.to_vec();
    oversized.resize(60_000, 0);
    socket.send_to(&oversized, address).await.unwrap();
    socket.send_to(BINDING, address).await.unwrap();
    let mut response = [0; 100];
    let (length, peer) = timeout(TEST_BUDGET, socket.recv_from(&mut response))
        .await
        .unwrap()
        .unwrap();
    assert_eq!(peer, address);
    assert_eq!(&response[..2], &[1, 1]);
    assert_eq!(&response[8..20], &BINDING[8..20]);
    assert_eq!(length, if address.is_ipv4() { 40 } else { 52 });
}

#[tokio::test]
async fn failed_listener_health_is_independent_of_siblings_and_relay_and_shutdown_joins_sockets() {
    let occupied = udp::bind("127.0.0.1:0".parse().unwrap()).unwrap();
    let service = Service::bind(config(vec![
        occupied.local_addr().unwrap(),
        "127.0.0.1:0".parse().unwrap(),
        "[::1]:0".parse().unwrap(),
    ]))
    .unwrap();
    let addresses = service.udp_addresses();
    assert_eq!(addresses.len(), 2);
    let admin = service.admin_address().unwrap();
    let statuses = Arc::clone(&service.statuses);
    let stop = CancellationToken::new();
    let stun_task = tokio::spawn(service.serve(stop.clone().cancelled_owned()));

    let directory = tempfile::tempdir().unwrap();
    let relay_listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let relay_address = relay_listener.local_addr().unwrap();
    let relay = Server::open(
        Endpoint::parse(&format!("ws://{relay_address}/v2/ws")).unwrap(),
        OriginPolicy::default(),
        &directory.path().join("relay.sqlite"),
    )
    .unwrap();
    let relay_stop = stop.clone();
    let relay_task = tokio::spawn(async move {
        relay
            .serve(relay_listener, relay_stop.cancelled_owned())
            .await
    });

    for address in &addresses {
        binding(*address).await;
    }
    assert!(http(admin, "/healthz").await.starts_with("HTTP/1.1 503"));
    assert!(
        http(relay_address, "/healthz")
            .await
            .starts_with("HTTP/1.1 200")
    );
    let metrics = http(admin, "/metrics").await;
    assert!(metrics.contains("windshare_stun_healthy{listener=\"udp-0\"} 0"));
    assert!(metrics.contains("windshare_stun_invalid_total{listener=\"udp-1\"} 1"));
    assert!(metrics.contains("windshare_stun_responded_total{listener=\"udp-2\"} 1"));
    let mut pending_admin = TcpStream::connect(admin).await.unwrap();
    pending_admin.write_all(b"GET /metrics").await.unwrap();

    stop.cancel();
    timeout(TEST_BUDGET, async {
        stun_task.await.unwrap();
        relay_task.await.unwrap().unwrap();
    })
    .await
    .unwrap();
    assert!(statuses.iter().all(|status| !status.healthy()));
    for address in addresses {
        udp::bind(address).unwrap();
    }
    TcpListener::bind(admin).await.unwrap();
    TcpListener::bind(relay_address).await.unwrap();
    let mut byte = [0];
    let closed = timeout(TEST_BUDGET, pending_admin.read(&mut byte))
        .await
        .unwrap();
    assert!(closed.is_err() || closed.unwrap() == 0);
}

#[tokio::test]
async fn admin_bind_failure_does_not_disable_udp_and_disabled_service_binds_nothing() {
    let occupied = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let mut config = config(vec!["127.0.0.1:0".parse().unwrap()]);
    config.admin_address = Some(occupied.local_addr().unwrap());
    let service = Service::bind(config.clone()).unwrap();
    assert!(service.admin_address().is_none());
    let address = service.udp_addresses()[0];
    let stop = CancellationToken::new();
    let task = tokio::spawn(service.serve(stop.clone().cancelled_owned()));
    binding(address).await;
    stop.cancel();
    timeout(TEST_BUDGET, task).await.unwrap().unwrap();
    udp::bind(address).unwrap();

    config.udp_addresses.clear();
    let service = Service::bind(config).unwrap();
    assert_eq!(service.udp_addresses(), Vec::<SocketAddr>::new());
    assert!(service.admin_address().is_none());
    service.serve(async {}).await;
}

#[tokio::test]
async fn total_udp_outage_keeps_admin_available_and_unstarted_drop_releases_binds() {
    let occupied = udp::bind("127.0.0.1:0".parse().unwrap()).unwrap();
    let service = Service::bind(config(vec![occupied.local_addr().unwrap()])).unwrap();
    assert_eq!(service.udp_addresses(), Vec::<SocketAddr>::new());
    let admin = service.admin_address().unwrap();
    let stop = CancellationToken::new();
    let task = tokio::spawn(service.serve(stop.clone().cancelled_owned()));
    assert!(http(admin, "/healthz").await.starts_with("HTTP/1.1 503"));
    stop.cancel();
    timeout(TEST_BUDGET, task).await.unwrap().unwrap();

    let service = Service::bind(config(vec!["127.0.0.1:0".parse().unwrap()])).unwrap();
    let udp = service.udp_addresses()[0];
    let admin = service.admin_address().unwrap();
    drop(service);
    udp::bind(udp).unwrap();
    TcpListener::bind(admin).await.unwrap();
}
