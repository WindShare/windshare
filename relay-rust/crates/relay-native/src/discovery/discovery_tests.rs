use super::*;
use crate::{
    Endpoint,
    node::{Identity, Runtime, Services, Settings},
};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD as B64};
use serde_json::{Value, json};
use windshare_relay_access::{descriptor::PublicRelay, trust};

fn signed(label: u8, purpose: Purpose) -> Envelope {
    let node = Runtime::default();
    node.identify(Identity::generate().unwrap()).unwrap();
    let ep = Endpoint::parse(&format!("wss://p{label}.example/v2/ws")).unwrap();
    serde_json::from_value(node.sign_descriptor(&ep, purpose).unwrap()).unwrap()
}

fn custom_envelope(
    label: u16,
    purpose: Purpose,
    now: u64,
    mutate: impl FnOnce(&mut Value),
) -> Envelope {
    let identity = Identity::generate().unwrap();
    let signing_key = identity.key();
    let key_b64 = B64.encode(signing_key.verifying_key().to_bytes());
    let endpoint = format!("wss://node{label}.example/v2/ws");
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
        "issued_at": now,
        "expires_at": now + purpose.lifetime(),
        "public_relay": PublicRelay::Enabled { anonymous: true },
        "authority": null,
    });
    mutate(&mut payload);
    let raw_payload = serde_json::to_vec(&payload).unwrap();
    let signed = trust::sign(purpose.domain(), &raw_payload, &signing_key);
    serde_json::from_value(json!({
        "version": 2,
        "descriptor": B64.encode(&raw_payload),
        "signature": B64.encode(&signed[raw_payload.len()..]),
    }))
    .unwrap()
}

#[test]
fn only_live_verified_peers_fit_address_and_prefix_budgets() {
    let now = crate::node::now().unsigned_abs();
    let mut book = Book::default();
    for (label, address, accepted) in [
        (1, "1.1.1.1", true),
        (2, "1.1.1.1", true),
        (3, "1.1.1.1", false),
        (4, "1.1.1.2", true),
        (5, "1.1.1.3", true),
        (6, "1.1.1.4", false),
        (7, "1.1.2.1", true),
    ] {
        let envelope = signed(label, Purpose::Gossip);
        assert!(envelope.clone().verify(Purpose::Live, now).is_err());
        assert!(
            envelope
                .clone()
                .verify(Purpose::Gossip, now + Purpose::Gossip.lifetime() + 1)
                .is_err()
        );
        let record = envelope.verify(Purpose::Gossip, now).unwrap();
        assert_eq!(
            book.insert(record, address.parse().unwrap(), now).is_ok(),
            accepted
        );
    }
    assert_eq!(book.sample(now, SAMPLE).len(), 5);
    assert!(book.sample(now, 0).is_empty());
    assert!(
        book.sample(now + Purpose::Gossip.lifetime() + 1, SAMPLE)
            .is_empty()
    );
    assert!(
        signed(8, Purpose::Live)
            .verify(Purpose::Gossip, now)
            .is_err()
    );
}

#[test]
fn peer_sampling_covers_full_book_uniformly_with_and_without_own_record() {
    use std::collections::BTreeMap;

    let now = crate::node::now().unsigned_abs();
    let (discovery, _receiver) = Discovery::new();
    let mut expected = BTreeMap::new();
    for index in 0..book::MAX_PEERS {
        let label = u8::try_from(index).unwrap();
        let envelope = signed(label, Purpose::Gossip);
        let serialized = serde_json::to_string(&envelope).unwrap();
        let record = envelope.verify(Purpose::Gossip, now).unwrap();
        let ip = [1, 1, label / 4, label % 4 + 1].into();
        discovery
            .book
            .lock()
            .unwrap()
            .insert(record, ip, now)
            .unwrap();
        expected.insert(serialized, 0usize);
    }
    assert_eq!(expected.len(), book::MAX_PEERS);

    let endpoint = Endpoint::parse("wss://self.example/v2/ws").unwrap();
    let identified = Runtime::default();
    identified.identify(Identity::generate().unwrap()).unwrap();
    let mut counts = expected.clone();
    for _ in 0..book::MAX_PEERS {
        let sample = discovery.sample_peers(now, &identified, &endpoint);
        assert_eq!(sample.len(), SAMPLE);
        for envelope in &sample[..SAMPLE - 1] {
            let key = serde_json::to_string(envelope).unwrap();
            *counts.get_mut(&key).expect("known book peer") += 1;
        }
        let own = sample[SAMPLE - 1]
            .clone()
            .verify(Purpose::Gossip, now)
            .unwrap();
        assert_eq!(own.endpoint, endpoint.as_str());
    }
    assert!(
        counts.values().all(|&seen| seen == SAMPLE - 1),
        "every peer in a full {}-peer book must appear uniformly",
        book::MAX_PEERS
    );

    let unidentified = Runtime::default();
    let mut full_counts = expected;
    let turns = book::MAX_PEERS / SAMPLE;
    for _ in 0..turns {
        let sample = discovery.sample_peers(now, &unidentified, &endpoint);
        assert_eq!(sample.len(), SAMPLE);
        for envelope in sample {
            let key = serde_json::to_string(&envelope).unwrap();
            *full_counts.get_mut(&key).expect("known book peer") += 1;
        }
    }
    assert!(
        full_counts.values().all(|&seen| seen == 1),
        "without own descriptor, full SAMPLE slots should cover every peer in one pass"
    );
}

#[test]
fn announcements_have_independent_global_and_source_budgets() {
    let mut book = Book::default();
    let remote = "8.8.8.8".parse().unwrap();
    for _ in 0..4 {
        assert!(book.allow(remote, 120));
    }
    assert!(!book.allow(remote, 120));
    for id in 1..=60 {
        assert!(book.allow([1, 1, 1, id].into(), 120));
    }
    assert!(!book.allow([9, 9, 9, 9].into(), 120));
    assert!(book.allow(remote, 180));
    for raw in [
        "http://relay.example",
        "https://127.0.0.1",
        "https://[::ffff:127.0.0.1]",
        "https://[2001:db8::1]",
        "https://100.64.1.1",
        "https://localhost",
    ] {
        assert!(transport::origin(raw).is_err(), "{raw}");
    }
    for raw in [
        "https://relay.example",
        "https://1.1.1.1",
        "https://[2606:4700:4700::1111]",
    ] {
        assert!(transport::origin(raw).is_ok());
    }
}

#[test]
fn verify_enforces_descriptor_service_stun_and_authority_invariants() {
    type PayloadMutator = fn(&mut Value);
    let now = crate::node::now().unsigned_abs();
    let invalid_cases: [(&str, PayloadMutator); 7] = [
        ("missing services", |p| {
            p.as_object_mut().unwrap().remove("services");
        }),
        ("missing stun", |p| {
            p.as_object_mut().unwrap().remove("stun");
        }),
        ("enabled public relay without relay service", |p| {
            p["services"] = json!({"relay": false, "stun": false});
            p["public_relay"] = json!(PublicRelay::Enabled { anonymous: true });
        }),
        ("stun service without stun URLs", |p| {
            p["services"] = json!({"relay": true, "stun": true});
            p["stun"] = json!([]);
        }),
        ("more than four stun URLs", |p| {
            p["services"] = json!({"relay": true, "stun": true});
            p["stun"] = json!([
                "stun:a.example:3478",
                "stun:b.example:3478",
                "stun:c.example:3478",
                "stun:d.example:3478",
                "stun:e.example:3478",
            ]);
        }),
        ("malformed authority issuer", |p| {
            p["authority"] = json!({"url": "https://api.example", "issuer": "bad-key"});
        }),
        ("all-zero authority issuer", |p| {
            p["authority"] = json!({
                "url": "https://api.example",
                "issuer": B64.encode([0u8; 32]),
            });
        }),
    ];

    for (index, (label, mutate)) in invalid_cases.into_iter().enumerate() {
        let envelope = custom_envelope(
            u16::try_from(index + 1).unwrap(),
            Purpose::Gossip,
            now,
            mutate,
        );
        assert!(
            envelope.verify(Purpose::Gossip, now).is_err(),
            "expected rejection for {label}"
        );
    }
}

#[test]
fn book_admits_only_contributing_community_nodes_offering_relay_or_stun() {
    let now = crate::node::now().unsigned_abs();
    let valid_issuer = B64.encode([7u8; 32]);
    let mut book = Book::default();

    let rejected: Vec<(&str, Envelope)> = vec![
        (
            "disabled even with STUN",
            custom_envelope(1, Purpose::Gossip, now, |p| {
                p["services"] = json!({"relay": true, "stun": true});
                p["stun"] = json!(["stun:node1.example:3478"]);
                p["public_relay"] = json!(PublicRelay::Disabled);
            }),
        ),
        (
            "authority-managed official node",
            custom_envelope(2, Purpose::Gossip, now, |p| {
                p["authority"] = json!({"url": "https://api.example", "issuer": valid_issuer});
                p["services"] = json!({"relay": true, "stun": true});
                p["stun"] = json!(["stun:node2.example:3478"]);
            }),
        ),
        (
            "budget exhausted without STUN",
            custom_envelope(3, Purpose::Gossip, now, |p| {
                p["public_relay"] = json!(PublicRelay::BudgetExhausted);
            }),
        ),
        (
            "authenticated-only without STUN",
            custom_envelope(4, Purpose::Gossip, now, |p| {
                p["public_relay"] = json!(PublicRelay::Enabled { anonymous: false });
            }),
        ),
    ];
    for (idx, (label, envelope)) in rejected.into_iter().enumerate() {
        let record = envelope.verify(Purpose::Gossip, now).unwrap();
        assert!(!record.offers_community_service(), "{label}");
        let ip = IpAddr::from([1, 2, u8::try_from(idx + 1).unwrap(), 1]);
        assert!(book.insert(record, ip, now).is_err(), "{label}");
    }

    let accepted: Vec<(&str, Envelope)> = vec![
        (
            "anonymous relay",
            custom_envelope(10, Purpose::Gossip, now, |_| {}),
        ),
        (
            "stun-only community node",
            custom_envelope(11, Purpose::Gossip, now, |p| {
                p["services"] = json!({"relay": false, "stun": true});
                p["stun"] = json!(["stun:node11.example:3478"]);
                p["public_relay"] = json!(PublicRelay::Unavailable);
            }),
        ),
        (
            "budget-exhausted relay with STUN",
            custom_envelope(12, Purpose::Gossip, now, |p| {
                p["services"] = json!({"relay": true, "stun": true});
                p["stun"] = json!(["stun:node12.example:3478"]);
                p["public_relay"] = json!(PublicRelay::BudgetExhausted);
            }),
        ),
    ];
    for (idx, (label, envelope)) in accepted.into_iter().enumerate() {
        let record = envelope.verify(Purpose::Gossip, now).unwrap();
        assert!(record.offers_community_service(), "{label}");
        let ip = IpAddr::from([2, 2, u8::try_from(idx + 1).unwrap(), 1]);
        assert!(book.insert(record, ip, now).is_ok(), "{label}");
    }
    assert_eq!(book.records(now).len(), 3);
}

#[test]
fn self_gossip_suppresses_non_public_opted_out_and_observer_nodes_without_breaking_live_descriptor()
{
    let now = crate::node::now();
    let public_ep = Endpoint::parse("wss://community.example/v2/ws").unwrap();
    let cleartext_ep = Endpoint::parse("ws://127.0.0.1:8484/v2/ws").unwrap();
    let loopback_tls_ep = Endpoint::parse("wss://127.0.0.1/v2/ws").unwrap();

    // Default contributing public HTTPS node advertises itself.
    let node = Runtime::default();
    node.identify(Identity::generate().unwrap()).unwrap();
    assert!(matches!(
        self_gossip(&node, &public_ep),
        SelfGossip::Advertise(_)
    ));

    // Cleartext ws:// and non-public wss:// stay local-only in gossip while keeping live descriptors.
    assert!(matches!(
        self_gossip(&node, &cleartext_ep),
        SelfGossip::LocalOnly
    ));
    assert!(matches!(
        self_gossip(&node, &loopback_tls_ep),
        SelfGossip::LocalOnly
    ));
    assert!(node.descriptor(&cleartext_ep).is_ok());

    // Opted-out self-hosted node stays silent even when STUN is bound, while live descriptor still reports Disabled.
    let mut disabled_settings = Settings {
        services: Services {
            relay: true,
            stun: true,
        },
        stun_urls: vec!["stun:community.example:3478".into()],
        ..Settings::default()
    };
    disabled_settings.access.contributing = false;
    node.update(disabled_settings, now).unwrap();
    assert!(matches!(
        self_gossip(&node, &public_ep),
        SelfGossip::OptedOut
    ));
    assert!(node.descriptor(&public_ep).is_ok());

    // STUN-only contributing node still advertises itself.
    let stun_only = Settings {
        services: Services {
            relay: false,
            stun: true,
        },
        stun_urls: vec!["stun:community.example:3478".into()],
        ..Settings::default()
    };
    node.update(stun_only, now).unwrap();
    assert!(matches!(
        self_gossip(&node, &public_ep),
        SelfGossip::Advertise(_)
    ));

    // Authenticated-only node without STUN acts as observer-only (no self-announcement).
    let mut auth_only = Settings {
        services: Services {
            relay: true,
            stun: false,
        },
        ..Settings::default()
    };
    auth_only.access.anonymous = false;
    node.update(auth_only, now).unwrap();
    assert!(matches!(
        self_gossip(&node, &public_ep),
        SelfGossip::ObserverOnly
    ));
}

#[test]
fn failed_exchange_demotes_oldest_peer_for_eviction_and_evicts_after_threshold() {
    let now = crate::node::now().unsigned_abs();
    let mut book = Book::default();

    // Fill the book with 128 distinct peers across distinct /24 prefixes.
    for i in 0..u16::try_from(book::MAX_PEERS).unwrap() {
        let record = custom_envelope(i + 1, Purpose::Gossip, now, |_| {})
            .verify(Purpose::Gossip, now)
            .unwrap();
        let ip = IpAddr::from([
            100 + u8::try_from(i / 64).unwrap(),
            u8::try_from(i % 64).unwrap(),
            1,
            1,
        ]);
        book.insert(record, ip, now + u64::from(i)).unwrap();
    }
    assert_eq!(book.targets(now).len(), book::MAX_PEERS);
    assert!(
        book.targets(now)
            .iter()
            .all(|target| !target.ends_with('/'))
    );

    // One transient failure followed by success restores seniority so healthy newcomer still loses.
    assert!(!book.note_exchange_failure("https://node1.example/"));
    book.note_exchange_success("https://node1.example");
    let newcomer1 = custom_envelope(200, Purpose::Gossip, now, |_| {})
        .verify(Purpose::Gossip, now)
        .unwrap();
    book.insert(newcomer1, IpAddr::from([150, 1, 1, 1]), now + 500)
        .unwrap();
    let targets = book.targets(now);
    assert!(targets.contains(&"https://node1.example".to_owned()));
    assert!(!targets.contains(&"https://node128.example".to_owned()));

    // A single unresolved failure demotes the oldest peer behind all healthy peers on full-book insert.
    assert!(!book.note_exchange_failure("https://node1.example"));
    let newcomer2 = custom_envelope(201, Purpose::Gossip, now, |_| {})
        .verify(Purpose::Gossip, now)
        .unwrap();
    book.insert(newcomer2, IpAddr::from([150, 2, 1, 1]), now + 600)
        .unwrap();
    let targets = book.targets(now);
    assert!(!targets.contains(&"https://node1.example".to_owned()));
    assert!(targets.contains(&"https://node200.example".to_owned()));

    // Repeated failures on a non-full book actively evict at MAX_EXCHANGE_FAILURES.
    let mut small = Book::default();
    let peer = custom_envelope(300, Purpose::Gossip, now, |_| {})
        .verify(Purpose::Gossip, now)
        .unwrap();
    small
        .insert(peer, IpAddr::from([151, 1, 1, 1]), now)
        .unwrap();
    for _ in 1..book::MAX_EXCHANGE_FAILURES {
        assert!(!small.note_exchange_failure("https://node300.example"));
        assert_eq!(small.records(now).len(), 1);
    }
    assert!(small.note_exchange_failure("https://node300.example"));
    assert!(small.records(now).is_empty());
}

async fn send_discovery_http(
    discovery: &Discovery,
    node: &Runtime,
    endpoint: &Endpoint,
    raw_request: &str,
) -> (StatusCode, Vec<u8>) {
    use hyper::{server::conn::http1, service::service_fn};
    use hyper_util::rt::TokioIo;
    use std::convert::Infallible;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    let (server, mut client) = tokio::io::duplex(64 * 1024);
    let source: IpAddr = "8.8.8.8".parse().unwrap();
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
    let head = std::str::from_utf8(&raw[..split]).unwrap();
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
        raw[split + 4..].to_vec(),
    )
}

#[tokio::test]
async fn http_handler_suppresses_disabled_and_cleartext_self_and_rejects_invalid_posts() {
    let now = crate::node::now().unsigned_abs();
    let public_ep = Endpoint::parse("wss://self.example/v2/ws").unwrap();
    let cleartext_ep = Endpoint::parse("ws://127.0.0.1:8484/v2/ws").unwrap();

    let node = Runtime::default();
    node.identify(Identity::generate().unwrap()).unwrap();
    let (discovery, mut receiver) = Discovery::new();

    let get_req = "GET /discovery/v1/peers HTTP/1.1\r\nHost: self.example\r\n\r\n";

    // Contributing public HTTPS node advertises itself in GET.
    let (status, body) = send_discovery_http(&discovery, &node, &public_ep, get_req).await;
    assert_eq!(status, StatusCode::OK);
    let entries: Vec<Envelope> = serde_json::from_slice(&body).unwrap();
    assert_eq!(entries.len(), 1);

    // Cleartext ws:// omits own from GET.
    let (status, body) = send_discovery_http(&discovery, &node, &cleartext_ep, get_req).await;
    assert_eq!(status, StatusCode::OK);
    let entries: Vec<Envelope> = serde_json::from_slice(&body).unwrap();
    assert!(entries.is_empty());

    // Opted-out Disabled node omits own from GET.
    let mut disabled_settings = Settings::default();
    disabled_settings.access.contributing = false;
    node.update(disabled_settings, crate::node::now()).unwrap();
    let (status, body) = send_discovery_http(&discovery, &node, &public_ep, get_req).await;
    assert_eq!(status, StatusCode::OK);
    let entries: Vec<Envelope> = serde_json::from_slice(&body).unwrap();
    assert!(entries.is_empty());

    // POST rejects Disabled and structurally invalid descriptors without queueing probes.
    let disabled_peer = custom_envelope(1, Purpose::Gossip, now, |p| {
        p["public_relay"] = json!(PublicRelay::Disabled);
        p["services"] = json!({"relay": true, "stun": true});
        p["stun"] = json!(["stun:node1.example:3478"]);
    });
    let invalid_peer = custom_envelope(2, Purpose::Gossip, now, |p| {
        p["services"] = json!({"relay": false, "stun": false});
        p["public_relay"] = json!(PublicRelay::Enabled { anonymous: true });
    });
    for rejected in [disabled_peer, invalid_peer] {
        let payload = serde_json::to_string(&rejected).unwrap();
        let req = format!(
            "POST /discovery/v1/peers HTTP/1.1\r\nHost: self.example\r\nContent-Length: {}\r\n\r\n{}",
            payload.len(),
            payload
        );
        let (status, _) = send_discovery_http(&discovery, &node, &public_ep, &req).await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert!(receiver.try_recv().is_err());
    }

    // POST accepts a valid contributing community descriptor and queues a probe.
    let valid_peer = custom_envelope(3, Purpose::Gossip, now, |_| {});
    let payload = serde_json::to_string(&valid_peer).unwrap();
    let req = format!(
        "POST /discovery/v1/peers HTTP/1.1\r\nHost: self.example\r\nContent-Length: {}\r\n\r\n{}",
        payload.len(),
        payload
    );
    let (status, _) = send_discovery_http(&discovery, &node, &public_ep, &req).await;
    assert_eq!(status, StatusCode::ACCEPTED);
    let queued = receiver.try_recv().unwrap();
    assert_eq!(queued.endpoint, "wss://node3.example/v2/ws");
}
