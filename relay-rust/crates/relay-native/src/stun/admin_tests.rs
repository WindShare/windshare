use super::*;
use http_body_util::BodyExt;
use tokio::io::{AsyncReadExt, AsyncWriteExt};

fn statuses() -> Statuses {
    vec![
        Arc::new(ListenerStatus::new(0)),
        Arc::new(ListenerStatus::new(1)),
    ]
    .into()
}

#[tokio::test]
async fn health_and_metrics_retain_failed_listener_ids() {
    let statuses = statuses();
    statuses[0].metrics.healthy.store(true, Ordering::Release);
    statuses[0].metrics.received.store(3, Ordering::Relaxed);
    let request = Request::builder().uri("/healthz").body(()).unwrap();
    let response = route(&request, &statuses);
    assert_eq!(response.status(), StatusCode::SERVICE_UNAVAILABLE);
    let body = response.into_body().collect().await.unwrap().to_bytes();
    assert_eq!(&body[..], b"udp-0 healthy=true\nudp-1 healthy=false\n");
    statuses[1].metrics.healthy.store(true, Ordering::Release);
    assert_eq!(route(&request, &statuses).status(), StatusCode::OK);
    assert_eq!(
        route(&request, &[]).status(),
        StatusCode::SERVICE_UNAVAILABLE
    );
    let response = route(
        &Request::builder().uri("/metrics").body(()).unwrap(),
        &statuses,
    );
    assert_eq!(
        response.headers()[header::CONTENT_TYPE],
        METRICS_CONTENT_TYPE
    );
    let body = response.into_body().collect().await.unwrap().to_bytes();
    let text = std::str::from_utf8(&body).unwrap();
    assert!(text.contains("windshare_stun_received_total{listener=\"udp-0\"} 3"));
    assert!(text.contains("windshare_stun_read_errors_total{listener=\"udp-1\"} 0"));
}

#[tokio::test]
async fn admin_routes_reject_bodies_and_support_head_without_a_body() {
    for (method, path, header, value, expected) in [
        ("GET", "/missing", "x-test", "", StatusCode::NOT_FOUND),
        (
            "POST",
            "/healthz",
            "x-test",
            "",
            StatusCode::METHOD_NOT_ALLOWED,
        ),
        (
            "GET",
            "/healthz",
            "content-length",
            "1",
            StatusCode::BAD_REQUEST,
        ),
        (
            "GET",
            "/metrics",
            "transfer-encoding",
            "chunked",
            StatusCode::BAD_REQUEST,
        ),
    ] {
        let request = Request::builder()
            .method(method)
            .uri(path)
            .header(header, value)
            .body(())
            .unwrap();
        let response = route(&request, &statuses());
        assert_eq!(response.status(), expected);
        if expected == StatusCode::METHOD_NOT_ALLOWED {
            assert_eq!(response.headers()[hyper::header::ALLOW], "GET, HEAD");
        }
    }
    let request = Request::builder()
        .method(Method::HEAD)
        .uri("/metrics")
        .body(())
        .unwrap();
    let response = route(&request, &statuses());
    assert_ne!(response.headers()[header::CONTENT_LENGTH], "0");
    assert!(
        response
            .into_body()
            .collect()
            .await
            .unwrap()
            .to_bytes()
            .is_empty()
    );
}

#[tokio::test(start_paused = true)]
async fn absolute_admin_budget_covers_silent_and_non_reading_clients() {
    let (server, _client) = tokio::io::duplex(1);
    let started = tokio::time::Instant::now();
    assert!(serve(server, statuses()).await.is_err());
    assert_eq!(started.elapsed(), REQUEST_TIMEOUT);

    let (server, mut client) = tokio::io::duplex(1);
    let task = tokio::spawn(serve(server, statuses()));
    client
        .write_all(b"GET /metrics HTTP/1.1\r\nHost: localhost\r\n\r\n")
        .await
        .unwrap();
    assert!(task.await.unwrap().is_err());
}

#[tokio::test]
async fn real_http_head_sends_no_body_and_closes_the_connection() {
    let (server, mut client) = tokio::io::duplex(4096);
    let task = tokio::spawn(serve(server, statuses()));
    client
        .write_all(b"HEAD /healthz HTTP/1.1\r\nHost: localhost\r\n\r\n")
        .await
        .unwrap();
    let mut response = String::new();
    client.read_to_string(&mut response).await.unwrap();
    task.await.unwrap().unwrap();
    assert!(response.starts_with("HTTP/1.1 503"));
    assert!(response.ends_with("\r\n\r\n"));
}
