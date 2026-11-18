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
