use super::*;
use http_body_util::Full;
use hyper::{Response as HyperResponse, StatusCode, body::Bytes, service::service_fn};
use hyper_util::rt::TokioIo;
use std::net::{Ipv4Addr, Ipv6Addr};
use tokio::net::TcpListener;

#[test]
fn select_public_addrs_balances_dual_stack_filters_private_and_caps_fanout() {
    let v6: Vec<SocketAddr> = (1..=6)
        .map(|n| {
            SocketAddr::new(
                IpAddr::V6(Ipv6Addr::new(0x2606, 0x4700, 0x4700, 0, 0, 0, 0, n)),
                443,
            )
        })
        .collect();
    let v4: Vec<SocketAddr> = (1..=5)
        .map(|n| SocketAddr::new(IpAddr::V4(Ipv4Addr::new(1, 1, 1, n)), 443))
        .collect();
    let mapped_v4_0 = SocketAddr::new(IpAddr::V6(Ipv4Addr::new(1, 1, 1, 1).to_ipv6_mapped()), 443);

    let input = vec![
        SocketAddr::new(IpAddr::V4(Ipv4Addr::LOCALHOST), 443),
        SocketAddr::new(IpAddr::V4(Ipv4Addr::new(100, 64, 0, 1)), 443),
        SocketAddr::new(
            IpAddr::V6(Ipv6Addr::new(0x2001, 0xdb8, 0, 0, 0, 0, 0, 1)),
            443,
        ),
        v6[0],
        v6[0],
        v6[1],
        v6[2],
        v6[3],
        v6[4],
        v6[5],
        mapped_v4_0,
        v4[0],
        v4[1],
        v4[2],
        v4[3],
        v4[4],
    ];

    let selected = select_public_addrs(input);
    assert_eq!(
        selected,
        vec![v6[0], v4[0], v6[1], v4[1], v6[2], v4[2], v6[3], v4[3]]
    );

    let v4_first = select_public_addrs([v4[0], v4[1], v6[0]]);
    assert_eq!(v4_first, vec![v4[0], v6[0], v4[1]]);

    let none = select_public_addrs([
        SocketAddr::new(IpAddr::V4(Ipv4Addr::LOCALHOST), 443),
        SocketAddr::new(IpAddr::V4(Ipv4Addr::new(192, 168, 1, 1)), 443),
    ]);
    assert_eq!(none, Vec::<SocketAddr>::new());
}

#[test]
fn origin_and_seed_origin_enforce_dns_and_root_path_rules() {
    for raw in [
        "https://relay.example",
        "https://relay.example/",
        "https://relay.example:8443/",
        "https://1.1.1.1",
        "https://[2606:4700:4700::1111]:8443",
    ] {
        assert!(origin(raw).is_ok(), "origin({raw})");
        assert!(seed_origin(raw).is_ok(), "seed_origin({raw})");
    }

    // Relay base URLs may carry subpath mounts while bootstrap seeds must be root origins.
    assert!(origin("https://relay.example/mount").is_ok());
    assert!(seed_origin("https://relay.example/mount").is_err());

    for raw in [
        "http://relay.example",
        "https://localhost",
        "https://..",
        "https://.example",
        "https://a..b",
        "https://-bad.example",
        "https://127.1",
        "https://relay.example?",
        "https://relay.example#",
        "https://relay.example:0",
        "https://relay.example:65536",
    ] {
        assert!(origin(raw).is_err(), "origin({raw})");
        assert!(seed_origin(raw).is_err(), "seed_origin({raw})");
    }
}

#[tokio::test]
async fn resolve_to_addrs_falls_back_from_ipv6_to_ipv4_and_reports_connected_ip() {
    let live_listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let live_addr = live_listener.local_addr().unwrap();
    let unreachable_v6 = SocketAddr::new(IpAddr::V6(Ipv6Addr::LOCALHOST), live_addr.port());
    let server = tokio::spawn(async move {
        if let Ok((stream, _)) = live_listener.accept().await {
            let io = TokioIo::new(stream);
            let _ = hyper::server::conn::http1::Builder::new()
                .serve_connection(
                    io,
                    service_fn(|_| async {
                        Ok::<_, std::io::Error>(
                            HyperResponse::builder()
                                .status(StatusCode::OK)
                                .header("connection", "close")
                                .body(Full::new(Bytes::from_static(b"ok")))
                                .unwrap(),
                        )
                    }),
                )
                .await;
        }
    });

    let client = Client::builder()
        .no_proxy()
        .timeout(TIMEOUT)
        .redirect(Policy::none())
        .resolve_to_addrs("peer.example", &[unreachable_v6, live_addr])
        .build()
        .unwrap();
    let response = client
        .get(format!("http://peer.example:{}/", live_addr.port()))
        .send()
        .await
        .unwrap();
    assert_eq!(response.remote_addr(), Some(live_addr));
    // The connected address is loopback, so verified_remote_ip must reject it.
    assert!(verified_remote_ip(&response).is_err());

    server.abort();
}
