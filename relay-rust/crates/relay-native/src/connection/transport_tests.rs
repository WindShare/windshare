use super::*;
use crate::{
    config::CONNECTION_ATTEMPTS,
    connection::{Capacity, ConnectionLimits},
    test_support::{Fixture, connection},
};
use std::net::{IpAddr, Ipv4Addr};
use windshare_relay_core::clock::UnixMillis;

const SOURCE: IpAddr = IpAddr::V4(Ipv4Addr::LOCALHOST);

struct DestructionProbe {
    capacity: Arc<Capacity>,
    business: bool,
}

impl Drop for DestructionProbe {
    fn drop(&mut self) {
        let attempt = self.capacity.accept(SOURCE, connection(2), UnixMillis(0));
        if self.business {
            assert!(
                attempt
                    .unwrap()
                    .inspect(&Fixture::new().bytes("registerInitB64"))
                    .is_err()
            );
        } else {
            assert!(attempt.is_err());
        }
    }
}

#[test]
fn unpolled_transport_retains_both_budgets_through_its_destructor() {
    for business in [false, true] {
        let capacity = Capacity::new(
            ConnectionLimits {
                business: 1,
                controls: 1,
                business_per_source: 1,
                controls_per_source: 1,
            },
            CONNECTION_ATTEMPTS,
            UnixMillis(0),
        )
        .unwrap();
        let admission = capacity
            .accept(SOURCE, connection(1), UnixMillis(0))
            .unwrap();
        if business {
            admission
                .inspect(&Fixture::new().bytes("registerInitB64"))
                .unwrap();
        }
        let socket = AdmittedSocket::new(
            DestructionProbe {
                capacity: Arc::clone(&capacity),
                business,
            },
            admission,
        );
        drop(socket);
        let replacement = capacity
            .accept(SOURCE, connection(3), UnixMillis(0))
            .unwrap();
        replacement
            .inspect(&Fixture::new().bytes("registerInitB64"))
            .unwrap();
    }
}
