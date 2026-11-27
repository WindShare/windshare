use super::*;
use crate::{
    Endpoint,
    node::{Identity, Runtime},
};

fn signed(label: u8, purpose: Purpose) -> Envelope {
    let node = Runtime::default();
    node.identify(Identity::generate().unwrap()).unwrap();
    let ep = Endpoint::parse(&format!("wss://p{label}.example/v2/ws")).unwrap();
    serde_json::from_value(node.sign_descriptor(&ep, purpose).unwrap()).unwrap()
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
