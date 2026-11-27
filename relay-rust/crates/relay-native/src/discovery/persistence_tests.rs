use super::super::{book::MAX_PEERS, record::tests::padded_gossip_envelope, *};
use crate::{Database, Endpoint, node::Identity};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD as B64};
use hyper::{server::conn::http1, service::service_fn};
use hyper_util::rt::TokioIo;
use serde_json::{Value, json};
use std::net::{Ipv4Addr, Ipv6Addr};
use tokio::net::TcpListener;
use windshare_relay_access::{descriptor::PublicRelay, trust};

fn signed_gossip(
    identity: &Identity,
    endpoint: &str,
    issued_at: u64,
    mutate: impl FnOnce(&mut Value),
) -> Envelope {
    let signing_key = identity.key();
    let key_b64 = B64.encode(signing_key.verifying_key().to_bytes());
    let mut payload = json!({
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
    mutate(&mut payload);
    let raw_payload = serde_json::to_vec(&payload).unwrap();
    let signed = trust::sign(Purpose::Gossip.domain(), &raw_payload, &signing_key);
    serde_json::from_value(json!({
        "version": 2,
        "descriptor": B64.encode(&raw_payload),
        "signature": B64.encode(&signed[raw_payload.len()..]),
    }))
    .unwrap()
}

fn fill_distinct_peers(
    book: &mut Book,
    count: usize,
    start_label: u16,
    now: u64,
    inserted_at: u64,
) {
    for i in 0..count {
        let idx = start_label + u16::try_from(i).unwrap();
        let id = Identity::generate().unwrap();
        let record = signed_gossip(
            &id,
            &format!("wss://filler{idx}.example/v2/ws"),
            now,
            |_| {},
        )
        .verify(Purpose::Gossip, now)
        .unwrap();
        let ip = IpAddr::from([
            100 + u8::try_from(idx / 64).unwrap(),
            u8::try_from(idx % 64).unwrap(),
            1,
            1,
        ]);
        book.insert(record, ip, inserted_at + u64::try_from(i).unwrap())
            .unwrap();
    }
}

#[test]
fn book_rejects_rollback_and_preserves_seniority_only_for_unchanged_endpoint_and_ip() {
    let now = crate::node::now().unsigned_abs();
    let mut book = Book::default();

    let veteran_id = Identity::generate().unwrap();
    let veteran_ep = "wss://veteran.example/v2/ws";
    let veteran_addr = IpAddr::from([198, 51, 100, 1]);
    let initial = signed_gossip(&veteran_id, veteran_ep, now - 20, |_| {})
        .verify(Purpose::Gossip, now)
        .unwrap();
    book.insert(initial, veteran_addr, now).unwrap();

    // Older and equal issued_at records for the same node key must be rejected.
    for stale_issued in [now - 30, now - 20] {
        let stale = signed_gossip(&veteran_id, veteran_ep, stale_issued, |_| {})
            .verify(Purpose::Gossip, now)
            .unwrap();
        assert!(book.insert(stale, veteran_addr, now + 5).is_err());
    }

    // Renewing with the same IP and endpoint preserves original seniority and clears failure marks.
    assert!(!book.note_exchange_failure("https://veteran.example"));
    let renewed = signed_gossip(&veteran_id, veteran_ep, now - 10, |_| {})
        .verify(Purpose::Gossip, now)
        .unwrap();
    book.insert(renewed, veteran_addr, now + 50).unwrap();

    // Moving IP or endpoint resets seniority to the renewal timestamp.
    let moved_ip_id = Identity::generate().unwrap();
    let moved_ip_ep = "wss://moved-ip.example/v2/ws";
    book.insert(
        signed_gossip(&moved_ip_id, moved_ip_ep, now - 20, |_| {})
            .verify(Purpose::Gossip, now)
            .unwrap(),
        IpAddr::from([198, 51, 101, 1]),
        now + 1,
    )
    .unwrap();
    book.insert(
        signed_gossip(&moved_ip_id, moved_ip_ep, now - 10, |_| {})
            .verify(Purpose::Gossip, now)
            .unwrap(),
        IpAddr::from([198, 51, 102, 1]),
        now + 500,
    )
    .unwrap();

    let relocated_id = Identity::generate().unwrap();
    let relocated_addr = IpAddr::from([198, 51, 103, 1]);
    book.insert(
        signed_gossip(
            &relocated_id,
            "wss://old-ep.example/v2/ws",
            now - 20,
            |_| {},
        )
        .verify(Purpose::Gossip, now)
        .unwrap(),
        relocated_addr,
        now + 2,
    )
    .unwrap();
    book.insert(
        signed_gossip(
            &relocated_id,
            "wss://new-ep.example/v2/ws",
            now - 10,
            |_| {},
        )
        .verify(Purpose::Gossip, now)
        .unwrap(),
        relocated_addr,
        now + 600,
    )
    .unwrap();

    // Fill the remaining slots with peers inserted between now+10 and now+134.
    fill_distinct_peers(&mut book, book::MAX_PEERS - 3, 1, now, now + 10);
    assert_eq!(book.targets(now).len(), book::MAX_PEERS);

    // Two newcomers evict the two reset-seniority peers (now+600, now+500) while veteran survives.
    for extra in 0..2u16 {
        let newcomer = signed_gossip(
            &Identity::generate().unwrap(),
            &format!("wss://extra{extra}.example/v2/ws"),
            now,
            |_| {},
        )
        .verify(Purpose::Gossip, now)
        .unwrap();
        book.insert(
            newcomer,
            IpAddr::from([203, 0, 113, u8::try_from(extra + 1).unwrap()]),
            now + 100 + u64::from(extra),
        )
        .unwrap();
    }
    let targets = book.targets(now);
    assert!(targets.contains(&"https://veteran.example".to_owned()));
    assert!(!targets.contains(&"https://new-ep.example".to_owned()));
    assert!(!targets.contains(&"https://moved-ip.example".to_owned()));
}

#[test]
fn book_enforces_ipv6_slash48_quota_and_isolates_ipv4_ipv6_prefixes() {
    let now = crate::node::now().unsigned_abs();
    let mut book = Book::default();

    for (label, address, accepted) in [
        (1, "2606:4700:4700::1", true),
        (2, "2606:4700:4700::1", true),
        (3, "2606:4700:4700::1", false),
        (4, "2606:4700:4700:1::1", true),
        (5, "2606:4700:4700:2::1", true),
        (6, "2606:4700:4700:3::1", false),
        (7, "2606:4700:4701::1", true),
    ] {
        let record = signed_gossip(
            &Identity::generate().unwrap(),
            &format!("wss://v6-{label}.example/v2/ws"),
            now,
            |_| {},
        )
        .verify(Purpose::Gossip, now)
        .unwrap();
        assert_eq!(
            book.insert(record, address.parse().unwrap(), now).is_ok(),
            accepted,
            "case {label} ({address})"
        );
    }

    // IPv4 0.0.1.x and IPv6 0000:0000:0001::x share numeric prefix value 1,
    // so address-family isolation must prevent cross-family quota collisions.
    let v4_sample = IpAddr::V4(Ipv4Addr::new(0, 0, 1, 1));
    let v6_sample = IpAddr::V6(Ipv6Addr::new(0, 0, 1, 0, 0, 0, 0, 1));
    assert_eq!(book::prefix(v4_sample), book::prefix(v6_sample));

    for host in 1..=4u8 {
        let record = signed_gossip(
            &Identity::generate().unwrap(),
            &format!("wss://v4-prefix-{host}.example/v2/ws"),
            now,
            |_| {},
        )
        .verify(Purpose::Gossip, now)
        .unwrap();
        book.insert(record, IpAddr::V4(Ipv4Addr::new(0, 0, 1, host)), now)
            .unwrap();
    }
    let v6_same_numeric_prefix = signed_gossip(
        &Identity::generate().unwrap(),
        "wss://v6-prefix-1.example/v2/ws",
        now,
        |_| {},
    )
    .verify(Purpose::Gossip, now)
    .unwrap();
    assert!(book.insert(v6_same_numeric_prefix, v6_sample, now).is_ok());
}

#[tokio::test]
async fn sqlite_cache_round_trips_and_restore_reprobes_only_valid_remote_community_peers() {
    let dir = tempfile::tempdir().unwrap();
    let self_ep = Endpoint::parse("wss://self.example/v2/ws").unwrap();
    let store = Database::open(&dir.path().join("node.sqlite"), self_ep.identity).unwrap();
    assert_eq!(store.cached_peers(1024).unwrap(), None);

    store.cache_peers(b"initial").unwrap();
    assert_eq!(
        store.cached_peers(1024).unwrap().as_deref(),
        Some(b"initial".as_slice())
    );
    store.cache_peers(b"updated-cache").unwrap();
    assert_eq!(
        store.cached_peers(1024).unwrap().as_deref(),
        Some(b"updated-cache".as_slice())
    );
    // Size ceiling rejects oversized cached rows without erroring.
    assert_eq!(store.cached_peers(4).unwrap(), None);

    let now = crate::node::now().unsigned_abs();
    let self_id = Identity::generate().unwrap();
    let self_key = B64.encode(self_id.key().verifying_key().to_bytes());
    let relay_peer_id = Identity::generate().unwrap();
    let stun_peer_id = Identity::generate().unwrap();

    let self_by_ep = signed_gossip(
        &Identity::generate().unwrap(),
        self_ep.as_str(),
        now,
        |_| {},
    );
    let self_by_key = signed_gossip(&self_id, "wss://alias.example/v2/ws", now, |_| {});
    let expired = signed_gossip(
        &Identity::generate().unwrap(),
        "wss://expired.example/v2/ws",
        now.saturating_sub(Purpose::Gossip.lifetime() + 60),
        |_| {},
    );
    let disabled = signed_gossip(
        &Identity::generate().unwrap(),
        "wss://disabled.example/v2/ws",
        now,
        |p| {
            p["public_relay"] = json!(PublicRelay::Disabled);
            p["services"] = json!({"relay": true, "stun": true});
            p["stun"] = json!(["stun:disabled.example:3478"]);
        },
    );
    let relay_peer = signed_gossip(&relay_peer_id, "wss://peer-a.example/v2/ws", now, |_| {});
    let stun_peer = signed_gossip(&stun_peer_id, "wss://peer-b.example/v2/ws", now, |p| {
        p["services"] = json!({"relay": false, "stun": true});
        p["stun"] = json!(["stun:peer-b.example:3478"]);
        p["public_relay"] = json!(PublicRelay::Unavailable);
    });

    let cached = serde_json::to_vec(&vec![
        self_by_ep,
        self_by_key,
        expired,
        disabled,
        relay_peer.clone(),
        relay_peer,
        stun_peer,
    ])
    .unwrap();
    store.cache_peers(&cached).unwrap();

    let (discovery, mut receiver) = Discovery::new();
    discovery
        .restore(store, Some(&self_key), &self_ep)
        .await
        .unwrap();

    // Restored records must never enter Book before passing live verification.
    assert!(discovery.book.lock().unwrap().records(now).is_empty());

    let first = receiver.try_recv().expect("peer-a queued for reprobe");
    let second = receiver.try_recv().expect("peer-b queued for reprobe");
    assert!(receiver.try_recv().is_err());
    assert_eq!(first.record.endpoint, "wss://peer-a.example/v2/ws");
    assert_eq!(second.record.endpoint, "wss://peer-b.example/v2/ws");
}

#[tokio::test]
async fn restore_accepts_full_book_of_maximal_envelopes_and_ignores_oversized_blob() {
    let now = crate::node::now().unsigned_abs();
    let self_ep = Endpoint::parse("wss://self.example/v2/ws").unwrap();
    let dir = tempfile::tempdir().unwrap();
    let db = Database::open(&dir.path().join("node.db"), self_ep.identity).unwrap();

    let envelopes: Vec<_> = (0..MAX_PEERS)
        .map(|idx| padded_gossip_envelope(idx, now, MAX_RECORD))
        .collect();
    let encoded = serde_json::to_vec(&envelopes).unwrap();
    assert!(encoded.len() <= json_array_bound(MAX_PEERS));
    db.cache_peers(&encoded).unwrap();

    let (discovery, mut receiver) = Discovery::new();
    let drain = tokio::spawn(async move {
        let mut count = 0usize;
        while receiver.recv().await.is_some() {
            count += 1;
        }
        count
    });
    discovery.restore(db.clone(), None, &self_ep).await.unwrap();
    drop(discovery);
    assert_eq!(drain.await.unwrap(), MAX_PEERS);

    let oversized = vec![b' '; json_array_bound(MAX_PEERS) + 1];
    db.cache_peers(&oversized).unwrap();
    assert!(
        db.cached_peers(json_array_bound(MAX_PEERS))
            .unwrap()
            .is_none()
    );
}

async fn serve_single_get(body: Vec<u8>) -> (url::Url, tokio::task::JoinHandle<()>) {
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
    let url = url::Url::parse(&format!("http://{addr}{PATH}")).unwrap();
    (url, server)
}

#[tokio::test]
async fn exchange_accepts_full_sample_of_maximal_envelopes_and_rejects_oversized_entry() {
    let now = crate::node::now().unsigned_abs();
    let self_ep = Endpoint::parse("wss://self.example/v2/ws").unwrap();
    let client = reqwest::Client::builder().no_proxy().build().unwrap();

    let maximal_sample: Vec<_> = (0..SAMPLE)
        .map(|idx| padded_gossip_envelope(idx, now, MAX_RECORD))
        .collect();
    let sample_body = serde_json::to_vec(&maximal_sample).unwrap();
    assert!(sample_body.len() > SAMPLE * MAX_RECORD);
    assert!(sample_body.len() <= json_array_bound(SAMPLE));

    let (url, server) = serve_single_get(sample_body).await;
    let (discovery, mut receiver) = Discovery::new();
    discovery
        .exchange_with_client(&client, &url, "https://peer.example", None, &self_ep, None)
        .await
        .unwrap();
    server.abort();

    let mut queued = 0usize;
    while receiver.try_recv().is_ok() {
        queued += 1;
    }
    assert_eq!(queued, SAMPLE);

    let oversized_entry = vec![padded_gossip_envelope(99, now, MAX_RECORD + 1)];
    let oversized_body = serde_json::to_vec(&oversized_entry).unwrap();
    let (url, server) = serve_single_get(oversized_body).await;
    let (discovery, mut receiver) = Discovery::new();
    discovery
        .exchange_with_client(&client, &url, "https://peer.example", None, &self_ep, None)
        .await
        .unwrap();
    server.abort();
    assert!(receiver.try_recv().is_err());
}
