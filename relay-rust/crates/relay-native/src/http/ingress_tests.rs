use super::*;
use crate::{connection::ConnectionLimits, test_support::connection};
use futures_util::StreamExt;
use std::time::Duration;
use tokio::io::{AsyncReadExt, DuplexStream};
use tokio_tungstenite::{
    WebSocketStream,
    tungstenite::{Message, protocol::Role},
};
use windshare_relay_core::share::AttemptRate;

const TEST_PIPE_BYTES: usize = MAX_HTTP_HEADER_BYTES * 4;
const UPGRADE_REQUEST: &str = "GET /base/v2/ws?share_id=AQEBAQEBAQEBAQEB HTTP/1.1\r\nHost: relay.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n";
const HEALTH_REQUEST: &str = "GET /healthz HTTP/1.1\r\nHost: relay.test\r\n\r\n";

fn peer() -> SocketAddr {
    "127.0.0.1:12345".parse().unwrap()
}

fn admission(ingress: &Ingress) -> Result<Arc<Admission>> {
    Ok(ingress
        .capacity
        .accept(peer().ip(), connection(1), ingress.environment.now())?)
}

fn ingress(burst: u32) -> Ingress {
    ingress_with_limits(
        burst,
        ConnectionLimits {
            business: 1,
            controls: 1,
            business_per_source: 1,
            controls_per_source: 1,
        },
    )
}

fn ingress_with_limits(burst: u32, limits: ConnectionLimits) -> Ingress {
    let environment = NativeEnvironment::new().unwrap();
    let capacity = Capacity::new(
        limits,
        AttemptRate {
            burst,
            refill: Duration::from_secs(3600),
        },
        environment.now(),
    )
    .unwrap();
    Ingress::new(
        Endpoint::parse("ws://relay.test/base/v2/ws").unwrap(),
        OriginPolicy::new(["https://app.example"], false).unwrap(),
        SourcePolicy::default(),
        capacity,
        environment,
    )
}

async fn exchange(
    ingress: &Ingress,
    request: &str,
) -> (String, Result<Option<Upgrade<DuplexStream>>>) {
    exchange_from(ingress, peer(), request).await
}

async fn exchange_from(
    ingress: &Ingress,
    peer: SocketAddr,
    request: &str,
) -> (String, Result<Option<Upgrade<DuplexStream>>>) {
    let (server, mut client) = tokio::io::duplex(TEST_PIPE_BYTES);
    client.write_all(request.as_bytes()).await.unwrap();
    let reader = async {
        let mut bytes = Vec::new();
        while !bytes.ends_with(b"\r\n\r\n") {
            bytes.push(client.read_u8().await.unwrap());
        }
        let mut response = String::from_utf8(bytes).unwrap();
        if response.starts_with("HTTP/1.1 101 ") {
            assert!(response.contains("connection: Upgrade\r\n"), "{response}");
            assert!(response.contains("upgrade: websocket\r\n"), "{response}");
        }
        if !request.starts_with("HEAD ") && !response.starts_with("HTTP/1.1 101 ") {
            let mut body = String::new();
            client.read_to_string(&mut body).await.unwrap();
            response.push_str(&body);
        }
        response
    };
    let (response, upgraded) = tokio::join!(reader, ingress.accept(server, peer, connection(1)));
    (response, upgraded)
}

#[tokio::test]
async fn http_routes_health_methods_and_canonical_upgrade_path() {
    let ingress = ingress(32);
    for (request, status) in [
        (HEALTH_REQUEST.to_owned(), 200),
        (
            "HEAD /healthz HTTP/1.1\r\nHost: relay.test\r\n\r\n".to_owned(),
            200,
        ),
        (
            "POST /healthz HTTP/1.1\r\nHost: relay.test\r\n\r\n".to_owned(),
            405,
        ),
        (
            "GET /missing HTTP/1.1\r\nHost: relay.test\r\n\r\n".to_owned(),
            404,
        ),
        (
            "GET /base/healthz HTTP/1.1\r\nHost: relay.test\r\n\r\n".to_owned(),
            404,
        ),
        (
            "GET /base/v2/ws?share_id=AQEBAQEBAQEBAQEB HTTP/1.1\r\nHost: relay.test\r\n\r\n"
                .to_owned(),
            400,
        ),
        (format!("{UPGRADE_REQUEST}Content-Length: 1\r\n\r\nx"), 400),
        (
            format!("{UPGRADE_REQUEST}Transfer-Encoding: chunked\r\n\r\n"),
            400,
        ),
        (
            format!("{}\r\n", UPGRADE_REQUEST.replace("/base/v2/ws", "/v2/ws")),
            404,
        ),
        (
            format!("{}\r\n", UPGRADE_REQUEST.replace("GET ", "HEAD ")),
            405,
        ),
        (
            format!(
                "{}\r\n",
                UPGRADE_REQUEST.replace("?share_id=AQEBAQEBAQEBAQEB", "")
            ),
            400,
        ),
    ] {
        let (response, upgraded) = exchange(&ingress, &request).await;
        assert!(
            response.starts_with(&format!("HTTP/1.1 {status} ")),
            "{response}"
        );
        assert!(upgraded.unwrap().is_none());
        if request.starts_with("HEAD ") {
            assert!(response.ends_with("\r\n\r\n"));
        } else if status == 200 {
            assert!(response.ends_with("ok\n"));
        }
        if status == 405 && request.contains("/healthz ") {
            assert!(response.contains("allow: GET, HEAD\r\n"));
        }
    }
}

#[tokio::test]
async fn health_and_rejections_do_not_consume_upgrade_attempts() {
    let ingress = ingress(1);
    for request in [
        HEALTH_REQUEST.to_owned(),
        format!("{UPGRADE_REQUEST}Origin: https://evil.example\r\n\r\n"),
        format!("{UPGRADE_REQUEST}Origin: \r\n\r\n"),
        format!(
            "{UPGRADE_REQUEST}Origin: https://app.example\r\nOrigin: https://app.example\r\n\r\n"
        ),
        format!(
            "{}\r\n",
            UPGRADE_REQUEST.replace("Sec-WebSocket-Version: 13", "Sec-WebSocket-Version: 12")
        ),
    ] {
        let (response, upgraded) = exchange(&ingress, &request).await;
        assert!(!response.starts_with("HTTP/1.1 101 "));
        assert!(upgraded.unwrap().is_none());
    }
    let (response, upgraded) = exchange(
        &ingress,
        &format!("{UPGRADE_REQUEST}Origin: https://app.example\r\n\r\n"),
    )
    .await;
    assert!(response.starts_with("HTTP/1.1 101 "));
    drop(upgraded.unwrap().unwrap());
    let (response, upgraded) = exchange(&ingress, &format!("{UPGRADE_REQUEST}\r\n")).await;
    assert!(response.starts_with("HTTP/1.1 503 "));
    assert!(upgraded.unwrap().is_none());
    let (response, _) = exchange(&ingress, HEALTH_REQUEST).await;
    assert!(response.starts_with("HTTP/1.1 200 "));
}

#[tokio::test]
async fn health_survives_full_business_and_control_budgets_and_upgrade_owns_its_slot() {
    let ingress = ingress(16);
    let business = admission(&ingress).unwrap();
    business
        .inspect(&crate::test_support::Fixture::new().bytes("registerInitB64"))
        .unwrap();
    let (response, upgraded) = exchange(&ingress, &format!("{UPGRADE_REQUEST}\r\n")).await;
    assert!(response.starts_with("HTTP/1.1 101 "));
    let upgraded = upgraded.unwrap().unwrap();
    assert!(admission(&ingress).is_err());
    let (response, _) = exchange(&ingress, HEALTH_REQUEST).await;
    assert!(response.starts_with("HTTP/1.1 200 "));
    let (response, refused) = exchange(&ingress, &format!("{UPGRADE_REQUEST}\r\n")).await;
    assert!(response.starts_with("HTTP/1.1 503 "));
    assert!(refused.unwrap().is_none());
    drop(upgraded);
    assert!(admission(&ingress).is_ok());
    drop(business);
}

#[tokio::test]
async fn upgrade_preserves_frames_read_with_request_headers() {
    let ingress = ingress(16);
    let (server, mut client) = tokio::io::duplex(TEST_PIPE_BYTES);
    let mut bytes = format!("{UPGRADE_REQUEST}\r\n").into_bytes();
    // Masked binary "ab" arrives in the same write as the HTTP request.
    bytes.extend_from_slice(&[0x82, 0x82, 1, 2, 3, 4, b'a' ^ 1, b'b' ^ 2]);
    client.write_all(&bytes).await.unwrap();
    let Upgrade {
        stream,
        buffered,
        admission,
        ..
    } = ingress
        .accept(server, peer(), connection(1))
        .await
        .unwrap()
        .unwrap();
    let mut socket =
        WebSocketStream::from_partially_read(stream, buffered, Role::Server, None).await;
    assert_eq!(
        socket.next().await.unwrap().unwrap(),
        Message::Binary(b"ab".to_vec().into())
    );
    drop(socket);
    drop(admission);
}

#[tokio::test]
async fn oversized_and_excessive_headers_are_bounded_before_admission() {
    let ingress = ingress(1);
    for headers in [
        format!("X-Padding: {}\r\n", "x".repeat(MAX_HTTP_HEADER_BYTES)),
        "X-Padding: x\r\n".repeat(MAX_HTTP_HEADERS),
    ] {
        let (response, upgraded) =
            exchange(&ingress, &format!("{UPGRADE_REQUEST}{headers}\r\n")).await;
        assert!(response.starts_with("HTTP/1.1 431 "), "{response}");
        assert!(upgraded.is_err());
    }
    assert!(admission(&ingress).is_ok());
}

#[tokio::test(start_paused = true)]
async fn incomplete_request_expires_on_the_http_deadline() {
    let ingress = ingress(1);
    let (server, mut client) = tokio::io::duplex(TEST_PIPE_BYTES);
    client
        .write_all(b"GET /healthz HTTP/1.1\r\n")
        .await
        .unwrap();
    let started = tokio::time::Instant::now();
    assert!(ingress.accept(server, peer(), connection(1)).await.is_err());
    assert_eq!(started.elapsed(), HTTP_REQUEST_TIMEOUT);
    assert_eq!(
        client.read_u8().await.unwrap_err().kind(),
        std::io::ErrorKind::UnexpectedEof
    );
    assert!(admission(&ingress).is_ok());
}

#[tokio::test(start_paused = true)]
async fn blocked_response_expires_and_releases_pending_upgrade_admission() {
    let ingress = ingress(16);
    for request in [HEALTH_REQUEST.to_owned(), format!("{UPGRADE_REQUEST}\r\n")] {
        let (server, mut client) = tokio::io::duplex(1);
        let started = tokio::time::Instant::now();
        let (upgraded, written) = tokio::join!(
            ingress.accept(server, peer(), connection(1)),
            client.write_all(request.as_bytes()),
        );
        written.unwrap();
        assert!(upgraded.is_err());
        assert_eq!(started.elapsed(), HTTP_REQUEST_TIMEOUT);
        assert!(admission(&ingress).is_ok());
    }
}

#[tokio::test]
async fn proxy_sources_are_isolated_and_invalid_identity_does_not_spend_admission() {
    let mut ingress = ingress_with_limits(
        3,
        ConnectionLimits {
            business: 2,
            controls: 2,
            business_per_source: 1,
            controls_per_source: 1,
        },
    );
    ingress.sources = SourcePolicy::trusted_proxy([peer().ip()], "cf-connecting-ip").unwrap();
    for headers in [
        "",
        "CF-Connecting-IP: invalid\r\n",
        "CF-Connecting-IP: 192.0.2.1, 192.0.2.2\r\n",
        "CF-Connecting-IP: 192.0.2.1\r\nCF-Connecting-IP: 192.0.2.2\r\n",
    ] {
        let (response, upgraded) =
            exchange(&ingress, &format!("{UPGRADE_REQUEST}{headers}\r\n")).await;
        assert!(response.starts_with("HTTP/1.1 400 "), "{response}");
        assert!(upgraded.unwrap().is_none());
    }
    let first_request = format!("{UPGRADE_REQUEST}CF-Connecting-IP: 192.0.2.1\r\n\r\n");
    let second_request = format!("{UPGRADE_REQUEST}CF-Connecting-IP: 2001:db8::1\r\n\r\n");
    let (response, first) = exchange(&ingress, &first_request).await;
    assert!(response.starts_with("HTTP/1.1 101 "));
    let (response, rejected) = exchange(&ingress, &first_request).await;
    assert!(response.starts_with("HTTP/1.1 503 "));
    assert!(rejected.unwrap().is_none());
    let (response, second) = exchange(&ingress, &second_request).await;
    assert!(response.starts_with("HTTP/1.1 101 "));
    let (response, _) = exchange(&ingress, HEALTH_REQUEST).await;
    assert!(response.starts_with("HTTP/1.1 200 "));
    drop(first.unwrap().unwrap());
    let (response, replacement) = exchange(&ingress, &first_request).await;
    assert!(response.starts_with("HTTP/1.1 101 "));
    drop(second.unwrap().unwrap());
    drop(replacement.unwrap().unwrap());
}

#[tokio::test]
async fn direct_upgrade_uses_socket_ip_without_port_or_forwarded_header_bypass() {
    let ingress = ingress_with_limits(
        8,
        ConnectionLimits {
            business: 2,
            controls: 2,
            business_per_source: 1,
            controls_per_source: 1,
        },
    );
    let (response, first) = exchange(&ingress, &format!("{UPGRADE_REQUEST}\r\n")).await;
    assert!(response.starts_with("HTTP/1.1 101 "));
    let forged = format!(
        "{UPGRADE_REQUEST}CF-Connecting-IP: 192.0.2.1\r\nX-Forwarded-For: 192.0.2.2\r\n\r\n"
    );
    let (response, rejected) =
        exchange_from(&ingress, "127.0.0.1:54321".parse().unwrap(), &forged).await;
    assert!(response.starts_with("HTTP/1.1 503 "));
    assert!(rejected.unwrap().is_none());
    let (response, other) =
        exchange_from(&ingress, "127.0.0.2:54321".parse().unwrap(), &forged).await;
    assert!(response.starts_with("HTTP/1.1 101 "));
    drop(first.unwrap().unwrap());
    drop(other.unwrap().unwrap());
}
