use super::*;
use crate::{
    Endpoint,
    node::{Identity, Runtime, Settings},
};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD as B64};
use serde_json::json;
use windshare_relay_access::{descriptor::PublicRelay, trust};

fn signed_peer(identity: &Identity, endpoint: &str, issued_at: u64) -> Envelope {
    let signing_key = identity.key();
    let key_b64 = B64.encode(signing_key.verifying_key().to_bytes());
    let payload = json!({
        "version": 2,
        "node_id": identity.id(),
        "node_key": key_b64,
        "relay_endpoint": endpoint,
        "services": { "relay": true, "stun": false },
        "stun": [],
        "zone": "global",
        "region": "",
        "operator": "",
        "provider": "",
        "platform": "native",
        "classes": ["owner", "standard", "lowest"],
        "issued_at": issued_at,
        "expires_at": issued_at + Purpose::Gossip.lifetime(),
        "public_relay": PublicRelay::Enabled { anonymous: true },
        "authority": null,
    });
    let raw_payload = serde_json::to_vec(&payload).unwrap();
    let signed = trust::sign(Purpose::Gossip.domain(), &raw_payload, &signing_key);
    serde_json::from_value(json!({
        "version": 2,
        "descriptor": B64.encode(&raw_payload),
        "signature": B64.encode(&signed[raw_payload.len()..]),
    }))
    .unwrap()
}

async fn send_http_from(
    discovery: &Discovery,
    node: &Runtime,
    endpoint: &Endpoint,
    source: IpAddr,
    raw_request: &str,
) -> (StatusCode, String, Vec<u8>) {
    use hyper::{server::conn::http1, service::service_fn};
    use hyper_util::rt::TokioIo;
    use std::convert::Infallible;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    let (server, mut client) = tokio::io::duplex(64 * 1024);
    let serve = http1::Builder::new().keep_alive(false).serve_connection(
        TokioIo::new(server),
        service_fn(|req| async move {
            Ok::<_, Infallible>(discovery.handle(req, source, node, endpoint).await)
        }),
    );
    let client_io = async {
        client.write_all(raw_request.as_bytes()).await.unwrap();
        let mut raw = Vec::new();
        client.read_to_end(&mut raw).await.unwrap();
        raw
    };
    let (http_result, raw) = tokio::join!(serve, client_io);
    http_result.unwrap();
    let split = raw
        .windows(4)
        .position(|w| w == b"\r\n\r\n")
        .expect("HTTP headers");
    let head = std::str::from_utf8(&raw[..split]).unwrap().to_owned();
    let code: u16 = head
        .lines()
        .next()
        .unwrap()
        .split_whitespace()
        .nth(1)
        .unwrap()
        .parse()
        .unwrap();
    (
        StatusCode::from_u16(code).unwrap(),
        head,
        raw[split + 4..].to_vec(),
    )
}

fn post_request(envelope: &Envelope) -> String {
    let payload = serde_json::to_string(envelope).unwrap();
    format!(
        "POST /discovery/v1/peers HTTP/1.1\r\nHost: self.example\r\nContent-Length: {}\r\n\r\n{}",
        payload.len(),
        payload
    )
}

#[tokio::test]
async fn post_deduplicates_self_known_and_in_flight_records_and_releases_on_drop() {
    let now = crate::node::now().unsigned_abs();
    let self_ep = Endpoint::parse("wss://self.example/v2/ws").unwrap();
    let self_id = Identity::generate().unwrap();
    let node = Runtime::default();
    node.identify(self_id.clone()).unwrap();
    let (discovery, mut receiver) = Discovery::new();

    // Self by endpoint or by node key must not consume a probe queue slot.
    let self_by_ep = signed_peer(&Identity::generate().unwrap(), self_ep.as_str(), now);
    let self_by_key = signed_peer(&self_id, "wss://alias.example/v2/ws", now);
    for (idx, record) in [self_by_ep, self_by_key].into_iter().enumerate() {
        let source = IpAddr::from([10, 0, 0, u8::try_from(idx + 1).unwrap()]);
        let (status, _, _) =
            send_http_from(&discovery, &node, &self_ep, source, &post_request(&record)).await;
        assert_eq!(status, StatusCode::ACCEPTED);
        assert!(receiver.try_recv().is_err());
    }

    // First announcement queues a probe; concurrent duplicate (even newer issued_at) is skipped while in-flight.
    let peer_id = Identity::generate().unwrap();
    let peer_t0 = signed_peer(&peer_id, "wss://peer1.example/v2/ws", now - 10);
    let peer_t1 = signed_peer(&peer_id, "wss://peer1.example/v2/ws", now - 5);
    let (status, _, _) = send_http_from(
        &discovery,
        &node,
        &self_ep,
        [20, 0, 0, 1].into(),
        &post_request(&peer_t0),
    )
    .await;
    assert_eq!(status, StatusCode::ACCEPTED);
    let in_flight_probe = receiver.try_recv().expect("initial probe queued");
    assert_eq!(in_flight_probe.record.endpoint, "wss://peer1.example/v2/ws");
    assert_eq!(
        in_flight_probe.source,
        ProbeSource::Ingress([20, 0, 0, 1].into())
    );

    let (status, _, _) = send_http_from(
        &discovery,
        &node,
        &self_ep,
        [20, 0, 0, 2].into(),
        &post_request(&peer_t1),
    )
    .await;
    assert_eq!(status, StatusCode::ACCEPTED);
    assert!(receiver.try_recv().is_err());

    // Dropping the probe releases the in-flight marker so subsequent announcements can queue again.
    drop(in_flight_probe);
    let (status, _, _) = send_http_from(
        &discovery,
        &node,
        &self_ep,
        [20, 0, 0, 3].into(),
        &post_request(&peer_t1),
    )
    .await;
    assert_eq!(status, StatusCode::ACCEPTED);
    let requeued = receiver.try_recv().expect("probe requeued after drop");

    // Once verified and stored in Book at t1, replaying <= t1 is skipped while > t1 queues.
    let verified = peer_t1.clone().verify(Purpose::Gossip, now).unwrap();
    discovery
        .book
        .lock()
        .unwrap()
        .insert(verified, [1, 1, 1, 1].into(), now)
        .unwrap();
    drop(requeued);

    for stale in [peer_t0, peer_t1] {
        let (status, _, _) = send_http_from(
            &discovery,
            &node,
            &self_ep,
            [20, 0, 0, 4].into(),
            &post_request(&stale),
        )
        .await;
        assert_eq!(status, StatusCode::ACCEPTED);
        assert!(receiver.try_recv().is_err());
    }

    let peer_t2 = signed_peer(&peer_id, "wss://peer1.example/v2/ws", now);
    let (status, _, _) = send_http_from(
        &discovery,
        &node,
        &self_ep,
        [20, 0, 0, 5].into(),
        &post_request(&peer_t2),
    )
    .await;
    assert_eq!(status, StatusCode::ACCEPTED);
    let refreshed = receiver.try_recv().expect("newer issued_at queued");
    assert_eq!(refreshed.record.issued, now);
}

#[tokio::test]
async fn post_queue_exhaustion_returns_503_and_get_sets_no_store_and_reuses_cached_own() {
    let now = crate::node::now().unsigned_abs();
    let self_ep = Endpoint::parse("wss://self.example/v2/ws").unwrap();
    let node = Runtime::default();
    node.identify(Identity::generate().unwrap()).unwrap();
    let (discovery, _receiver) = Discovery::new();

    let get_req = "GET /discovery/v1/peers HTTP/1.1\r\nHost: self.example\r\n\r\n";
    let (status, head, first_body) =
        send_http_from(&discovery, &node, &self_ep, [8, 8, 8, 8].into(), get_req).await;
    assert_eq!(status, StatusCode::OK);
    assert!(
        head.to_ascii_lowercase()
            .contains("cache-control: no-store\r\n"),
        "{head}"
    );
    let (_, _, second_body) =
        send_http_from(&discovery, &node, &self_ep, [8, 8, 4, 4].into(), get_req).await;
    assert_eq!(second_body, first_body);

    // Fill the 32-slot probe queue across distinct sources so rate limits are not hit.
    let mut queued_envelopes = Vec::with_capacity(QUEUE);
    for idx in 0..QUEUE {
        let id = Identity::generate().unwrap();
        let envelope = signed_peer(&id, &format!("wss://p{idx}.example/v2/ws"), now);
        let source = IpAddr::from([30, 0, 0, u8::try_from(idx + 1).unwrap()]);
        let (status, _, _) = send_http_from(
            &discovery,
            &node,
            &self_ep,
            source,
            &post_request(&envelope),
        )
        .await;
        assert_eq!(status, StatusCode::ACCEPTED, "slot {idx}");
        queued_envelopes.push(envelope);
    }

    // Replaying an already in-flight peer while the queue is full stays idempotent (202).
    let (status, _, _) = send_http_from(
        &discovery,
        &node,
        &self_ep,
        [30, 0, 1, 1].into(),
        &post_request(&queued_envelopes[0]),
    )
    .await;
    assert_eq!(status, StatusCode::ACCEPTED);

    // A genuinely new peer hitting the full probe queue receives 503 with Retry-After and no-store.
    let overflow = signed_peer(
        &Identity::generate().unwrap(),
        "wss://overflow.example/v2/ws",
        now,
    );
    let (status, head, _) = send_http_from(
        &discovery,
        &node,
        &self_ep,
        [30, 0, 1, 2].into(),
        &post_request(&overflow),
    )
    .await;
    assert_eq!(status, StatusCode::SERVICE_UNAVAILABLE);
    let lower = head.to_ascii_lowercase();
    assert!(lower.contains("retry-after:"), "{head}");
    assert!(lower.contains("cache-control: no-store\r\n"), "{head}");
}

#[tokio::test]
async fn announce_reads_hot_reloaded_bootstrap_nodes_live() {
    let self_ep = Endpoint::parse("wss://self.example/v2/ws").unwrap();
    let node = Runtime::default();
    node.identify(Identity::generate().unwrap()).unwrap();
    let (discovery, _receiver) = Discovery::new();

    // Start with no seeds; announce() sleeps until bootstrap_nodes updates.
    let mut seeds_watch = node.subscribe_bootstrap_nodes();
    assert!(seeds_watch.borrow_and_update().is_empty());

    let updated = Settings {
        bootstrap_nodes: vec!["https://new-seed.example".into()],
        ..Settings::default()
    };
    node.update(updated, crate::node::now()).unwrap();

    tokio::time::timeout(Duration::from_secs(1), seeds_watch.changed())
        .await
        .expect("bootstrap_nodes watch wakeup")
        .unwrap();
    let seeds = Arc::clone(&*seeds_watch.borrow_and_update());
    assert_eq!(
        discovery.exchange_targets(crate::node::now().unsigned_abs(), &self_ep, &seeds),
        ["https://new-seed.example"]
    );
}

#[tokio::test]
async fn exchange_skips_known_self_and_duplicate_peers_from_neighbor_sample() {
    use hyper::{server::conn::http1, service::service_fn};
    use hyper_util::rt::TokioIo;
    use tokio::net::TcpListener;

    let now = crate::node::now().unsigned_abs();
    let self_ep = Endpoint::parse("wss://self.example/v2/ws").unwrap();
    let self_id = Identity::generate().unwrap();
    let self_key = B64.encode(self_id.key().verifying_key().to_bytes());
    let known_id = Identity::generate().unwrap();
    let fresh_id = Identity::generate().unwrap();

    let self_env = signed_peer(&self_id, self_ep.as_str(), now);
    let known_env = signed_peer(&known_id, "wss://known.example/v2/ws", now);
    let fresh_env = signed_peer(&fresh_id, "wss://fresh.example/v2/ws", now);

    let (discovery, mut receiver) = Discovery::new();
    discovery
        .book
        .lock()
        .unwrap()
        .insert(
            known_env.clone().verify(Purpose::Gossip, now).unwrap(),
            [1, 1, 1, 1].into(),
            now,
        )
        .unwrap();

    // Sample includes self, an already-known peer, and the same fresh peer twice.
    let body =
        serde_json::to_vec(&vec![self_env, known_env, fresh_env.clone(), fresh_env]).unwrap();
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let server = tokio::spawn(async move {
        if let Ok((stream, _)) = listener.accept().await {
            let io = TokioIo::new(stream);
            let _ = http1::Builder::new()
                .serve_connection(
                    io,
                    service_fn(move |_| {
                        let body = body.clone();
                        async move {
                            Ok::<_, std::io::Error>(
                                Response::builder()
                                    .status(StatusCode::OK)
                                    .header(header::CONTENT_TYPE, "application/json")
                                    .header(header::CONNECTION, "close")
                                    .body(Full::new(Bytes::from(body)))
                                    .unwrap(),
                            )
                        }
                    }),
                )
                .await;
        }
    });

    let client = reqwest::Client::builder().no_proxy().build().unwrap();
    let url = url::Url::parse(&format!("http://{addr}{PATH}")).unwrap();
    discovery
        .exchange_with_client(
            &client,
            &url,
            "https://neighbor.example",
            Some(&self_key),
            &self_ep,
            None,
        )
        .await
        .unwrap();

    let queued = receiver.try_recv().expect("fresh peer queued once");
    assert_eq!(queued.record.endpoint, "wss://fresh.example/v2/ws");
    assert!(receiver.try_recv().is_err());
    server.abort();
}
