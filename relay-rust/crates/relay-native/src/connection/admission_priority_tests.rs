use super::*;
use crate::test_support::{Fixture, connection};
use std::time::Duration;

#[test]
fn anonymous_pressure_preserves_authenticated_then_owner_control_and_business_capacity() {
    let fixture = Fixture::new();
    for business in [false, true] {
        let capacity = Capacity::new(
            ConnectionLimits {
                business: 32,
                controls: 32,
                business_per_source: 32,
                controls_per_source: 32,
            },
            AttemptRate {
                burst: 128,
                refill: Duration::from_secs(1),
            },
            UnixMillis(0),
        )
        .unwrap();
        let connect = |priority, id| -> Result<Arc<Admission>, Refusal> {
            let slot = capacity.accept(
                "192.0.2.1".parse().unwrap(),
                connection(id),
                UnixMillis(0),
                priority,
            )?;
            if business {
                slot.inspect(&fixture.bytes("registerInitB64"))?;
            }
            Ok(slot)
        };
        let mut held = Vec::new();
        for (priority, count) in [
            (AdmissionPriority::Anonymous, 24),
            (AdmissionPriority::Authenticated, 4),
            (AdmissionPriority::Owner, 4),
        ] {
            for _ in 0..count {
                held.push(connect(priority, u64::try_from(held.len() + 1).unwrap()).unwrap());
            }
            assert!(
                connect(priority, 100).is_err(),
                "priority={priority:?} business={business}"
            );
        }
        drop(held);
        assert!(connect(AdmissionPriority::Anonymous, 101).is_ok());
    }
}

#[test]
fn anonymous_attempt_churn_preserves_authenticated_then_owner_upgrade_rate() {
    let capacity = Capacity::new(
        ConnectionLimits {
            business: 32,
            controls: 32,
            business_per_source: 32,
            controls_per_source: 32,
        },
        AttemptRate {
            burst: 32,
            refill: Duration::from_millis(100),
        },
        UnixMillis(0),
    )
    .unwrap();
    let mut next_id = 1u64;
    let mut churn = |priority, now| -> Result<(), Refusal> {
        let id = next_id;
        next_id += 1;
        let slot = capacity.accept("192.0.2.1".parse().unwrap(), connection(id), now, priority)?;
        drop(slot);
        Ok(())
    };

    for (priority, count) in [
        (AdmissionPriority::Anonymous, 24),
        (AdmissionPriority::Authenticated, 4),
        (AdmissionPriority::Owner, 4),
    ] {
        for _ in 0..count {
            assert!(
                churn(priority, UnixMillis(0)).is_ok(),
                "priority={priority:?}"
            );
        }
        assert_eq!(
            churn(priority, UnixMillis(0)),
            Err(Refusal::AttemptRate),
            "priority={priority:?}"
        );
    }

    // Under sustained anonymous pressure, single refills stay exclusively with Owner.
    assert_eq!(
        churn(AdmissionPriority::Anonymous, UnixMillis(100)),
        Err(Refusal::AttemptRate)
    );
    assert_eq!(
        churn(AdmissionPriority::Authenticated, UnixMillis(100)),
        Err(Refusal::AttemptRate)
    );
    assert!(churn(AdmissionPriority::Owner, UnixMillis(100)).is_ok());

    // Once Owner stops consuming, refills unlock Authenticated before Anonymous.
    assert_eq!(
        churn(AdmissionPriority::Anonymous, UnixMillis(600)),
        Err(Refusal::AttemptRate)
    );
    assert!(churn(AdmissionPriority::Authenticated, UnixMillis(600)).is_ok());

    // After full headroom recovers, Anonymous is admitted again.
    assert!(churn(AdmissionPriority::Anonymous, UnixMillis(1_100)).is_ok());
}
#[test]
fn lowering_public_business_limit_preserves_existing_and_owner_connections() {
    use crate::node::access::{Policy, Service};
    let service = Service::default();
    let limit = |n| {
        service
            .configure(Policy {
                public_connections: n,
                ..Default::default()
            })
            .unwrap();
    };
    limit(2);
    let capacity = Capacity::new(
        ConnectionLimits::default(),
        AttemptRate {
            burst: 64,
            refill: Duration::from_secs(1),
        },
        UnixMillis(0),
    )
    .unwrap();
    assert!(capacity.access.set(service.clone()).is_ok());
    let init = Fixture::new().bytes("registerInitB64");
    let open = |id, priority| -> Result<_, Refusal> {
        let a = capacity.accept([1, 1, 1, 1].into(), connection(id), UnixMillis(0), priority)?;
        a.inspect(&init)?;
        Ok(a)
    };
    let first = open(1, AdmissionPriority::Anonymous).unwrap();
    let second = open(2, AdmissionPriority::Authenticated).unwrap();
    limit(1);
    first.inspect(&init).unwrap();
    assert!(matches!(
        open(3, AdmissionPriority::Anonymous),
        Err(Refusal::PublicBusiness)
    ));
    let owner = open(4, AdmissionPriority::Owner).unwrap();
    drop(first);
    assert!(matches!(
        open(5, AdmissionPriority::Anonymous),
        Err(Refusal::PublicBusiness)
    ));
    drop(second);
    assert!(open(6, AdmissionPriority::Anonymous).is_ok());
    drop(owner);
}
