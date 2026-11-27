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
    assert_eq!(book.sample(now).len(), 5);
    assert!(book.sample(now + Purpose::Gossip.lifetime() + 1).is_empty());
    assert!(
        signed(8, Purpose::Live)
            .verify(Purpose::Gossip, now)
            .is_err()
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
