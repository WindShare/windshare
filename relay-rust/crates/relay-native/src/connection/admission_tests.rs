use super::*;
use crate::test_support::{Fixture, connection};
use std::time::Duration;

const SOURCE_A: IpAddr = IpAddr::V4(std::net::Ipv4Addr::new(192, 0, 2, 1));
const SOURCE_B: IpAddr = IpAddr::V6(std::net::Ipv6Addr::LOCALHOST);

fn capacity() -> Arc<Capacity> {
    Capacity::new(
        ConnectionLimits {
            business: 2,
            controls: 2,
            business_per_source: 1,
            controls_per_source: 1,
        },
        AttemptRate {
            burst: 8,
            refill: Duration::from_millis(100),
        },
        UnixMillis(0),
    )
    .unwrap()
}

#[test]
fn full_business_capacity_preserves_bounded_stop_ingress() {
    let fixture = Fixture::new();
    let capacity = capacity();
    let first = capacity
        .accept(SOURCE_A, connection(1), UnixMillis(0))
        .unwrap();
    let second = capacity
        .accept(SOURCE_B, connection(2), UnixMillis(0))
        .unwrap();
    first.inspect(&fixture.bytes("registerInitB64")).unwrap();
    second.inspect(&fixture.bytes("registerInitB64")).unwrap();
    assert_eq!(
        capacity.state.lock().unwrap().total,
        Usage {
            business: 2,
            controls: 0
        }
    );
    let stop = capacity
        .accept(SOURCE_A, connection(3), UnixMillis(0))
        .unwrap();
    stop.inspect(&fixture.bytes("stopInitB64")).unwrap();
    let competing = capacity
        .accept(SOURCE_B, connection(4), UnixMillis(0))
        .unwrap();
    assert_eq!(
        competing.inspect(&fixture.bytes("registerInitB64")),
        Err(Refusal::GlobalBusiness),
    );
    let other = "192.0.2.2".parse().unwrap();
    assert!(matches!(
        capacity.accept(other, connection(5), UnixMillis(0)),
        Err(Refusal::GlobalControl)
    ));
    assert!(!capacity.state.lock().unwrap().sources.contains_key(&other));
    drop(competing);
    drop(stop);
    assert_eq!(
        capacity.state.lock().unwrap().total,
        Usage {
            business: 2,
            controls: 0
        }
    );
    drop(first);
    drop(second);
    assert!(capacity.state.lock().unwrap().sources.is_empty());
}

#[test]
fn source_rejections_preserve_other_sources_and_do_not_partially_charge_budgets() {
    let fixture = Fixture::new();
    let capacity = capacity();
    let first = capacity
        .accept(SOURCE_A, connection(1), UnixMillis(0))
        .unwrap();
    assert!(matches!(
        capacity.accept(SOURCE_A, connection(2), UnixMillis(0)),
        Err(Refusal::SourceControl)
    ));
    let second = capacity
        .accept(SOURCE_B, connection(3), UnixMillis(0))
        .unwrap();
    first.inspect(&fixture.bytes("registerInitB64")).unwrap();
    first.inspect(&fixture.bytes("registerInitB64")).unwrap();
    let competing = capacity
        .accept(SOURCE_A, connection(4), UnixMillis(0))
        .unwrap();
    assert_eq!(
        competing.inspect(&fixture.bytes("registerInitB64")),
        Err(Refusal::SourceBusiness)
    );
    second.inspect(&fixture.bytes("registerInitB64")).unwrap();
    assert_eq!(
        capacity.state.lock().unwrap().total,
        Usage {
            business: 2,
            controls: 1
        }
    );
    drop(first);
    competing
        .inspect(&fixture.bytes("registerInitB64"))
        .unwrap();
    drop(second);
    drop(competing);
    assert_eq!(capacity.state.lock().unwrap().total, Usage::default());
    assert!(capacity.state.lock().unwrap().sources.is_empty());
    for id in 5..10 {
        drop(
            capacity
                .accept(SOURCE_A, connection(id), UnixMillis(0))
                .unwrap(),
        );
    }
    assert!(matches!(
        capacity.accept(SOURCE_B, connection(10), UnixMillis(0)),
        Err(Refusal::AttemptRate)
    ));
    assert!(capacity.state.lock().unwrap().sources.is_empty());
}

#[test]
fn socket_descendants_keep_both_budgets_until_the_last_owner_is_destroyed() {
    let fixture = Fixture::new();
    for business in [false, true] {
        let capacity = capacity();
        let supervisor = capacity
            .accept(SOURCE_A, connection(1), UnixMillis(0))
            .unwrap();
        if business {
            supervisor
                .inspect(&fixture.bytes("registerInitB64"))
                .unwrap();
        }
        let writer = Arc::clone(&supervisor);
        let reader = Arc::clone(&supervisor);
        drop(supervisor);
        drop(reader);
        let expected = Usage {
            business: usize::from(business),
            controls: usize::from(!business),
        };
        assert_eq!(capacity.state.lock().unwrap().total, expected);
        assert_eq!(capacity.state.lock().unwrap().sources[&SOURCE_A], expected);
        drop(writer);
        assert_eq!(capacity.state.lock().unwrap().total, Usage::default());
        assert!(capacity.state.lock().unwrap().sources.is_empty());
        let replacement = capacity
            .accept(SOURCE_A, connection(2), UnixMillis(0))
            .unwrap();
        replacement
            .inspect(&fixture.bytes("registerInitB64"))
            .unwrap();
    }
}

#[test]
fn closing_does_not_refund_the_attempt_rate() {
    let capacity = capacity();
    for id in 1..=8 {
        drop(
            capacity
                .accept(SOURCE_A, connection(id), UnixMillis(0))
                .unwrap(),
        );
    }
    assert!(matches!(
        capacity.accept(SOURCE_B, connection(9), UnixMillis(0)),
        Err(Refusal::AttemptRate)
    ));
    assert!(matches!(
        capacity.accept(SOURCE_B, connection(10), UnixMillis(99)),
        Err(Refusal::AttemptRate)
    ));
    assert_eq!(capacity.state.lock().unwrap().total, Usage::default());
    assert!(capacity.state.lock().unwrap().sources.is_empty());
    assert!(
        capacity
            .accept(SOURCE_B, connection(11), UnixMillis(100))
            .is_ok()
    );
}

#[test]
fn mapped_ipv4_cannot_create_a_second_source_budget() {
    let capacity = capacity();
    let _first = capacity
        .accept(SOURCE_A, connection(1), UnixMillis(0))
        .unwrap();
    assert!(matches!(
        capacity.accept(
            "::ffff:192.0.2.1".parse().unwrap(),
            connection(2),
            UnixMillis(0)
        ),
        Err(Refusal::SourceControl)
    ));
    assert_eq!(capacity.state.lock().unwrap().sources.len(), 1);
}
