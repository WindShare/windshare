use super::*;
use crate::test_support::{Fixture, connection};
use std::time::Duration;

fn capacity() -> Arc<Capacity> {
    Capacity::new(
        1,
        2,
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
    let business = capacity.accept(UnixMillis(0)).unwrap();
    business
        .inspect(&fixture.bytes("registerInitB64"), connection(1))
        .unwrap();
    assert_eq!(capacity.business.available_permits(), 0);
    assert_eq!(capacity.controls.available_permits(), 2);
    let stop = capacity.accept(UnixMillis(0)).unwrap();
    stop.inspect(&fixture.bytes("stopInitB64"), connection(2))
        .unwrap();
    let competing = capacity.accept(UnixMillis(0)).unwrap();
    assert!(
        competing
            .inspect(&fixture.bytes("registerInitB64"), connection(3))
            .is_err()
    );
    assert!(capacity.accept(UnixMillis(0)).is_err());
    drop(competing);
    drop(stop);
    assert_eq!(capacity.controls.available_permits(), 2);
    assert_eq!(capacity.business.available_permits(), 0);
    drop(business);
    assert_eq!(capacity.business.available_permits(), 1);
}

#[test]
fn socket_descendants_keep_the_slot_until_the_last_owner_is_destroyed() {
    let fixture = Fixture::new();
    let capacity = capacity();
    let supervisor = capacity.accept(UnixMillis(0)).unwrap();
    supervisor
        .inspect(&fixture.bytes("registerInitB64"), connection(1))
        .unwrap();
    let writer = Arc::clone(&supervisor);
    let reader = Arc::clone(&supervisor);
    drop(supervisor);
    drop(reader);
    assert_eq!(capacity.business.available_permits(), 0);
    drop(writer);
    assert_eq!(capacity.business.available_permits(), 1);
}

#[test]
fn closing_does_not_refund_the_attempt_rate() {
    let capacity = capacity();
    for _ in 0..8 {
        drop(capacity.accept(UnixMillis(0)).unwrap());
    }
    assert!(capacity.accept(UnixMillis(0)).is_err());
    assert!(capacity.accept(UnixMillis(99)).is_err());
    assert!(capacity.accept(UnixMillis(100)).is_ok());
}
