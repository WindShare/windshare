use super::*;

fn subject() -> Subject {
    Subject {
        device: "device".into(),
        session: "session".into(),
        path: "path".into(),
    }
}
fn grant() -> Grant {
    Grant {
        urls: vec!["turn:turn.cloudflare.com:3478".into()],
        username: "user".into(),
        password: "password".into(),
    }
}
fn lease() -> Lease {
    Lease::new("lease".into(), "service".into(), "key".into(), subject(), 0)
}

#[test]
fn concurrent_withdrawal_never_publishes_provider_result() {
    for (authorized, withdraw, now) in [
        (false, false, 1),
        (true, true, 1),
        (true, false, ISSUANCE_DEADLINE_MS),
    ] {
        let mut lease = lease();
        if withdraw {
            lease.withdraw(0);
        }
        assert!(!lease.issued(grant(), authorized, now));
        assert!(lease.pending());
        lease.settle_expiry(lease.expires_at);
        assert_eq!(lease.state, LeaseState::Expired);
    }
}
#[test]
fn renewal_requires_live_device_and_session_ownership() {
    let mut lease = lease();
    assert!(lease.issued(grant(), true, 1));
    assert!(lease.renewable_by("device", "session", 2));
    assert!(!lease.renewable_by("other", "session", 2));
    assert!(!lease.renewable_by("device", "other", 2));
    assert!(!lease.renewable_by("device", "session", LEASE_LIFETIME_MS));
    assert!(lease.owned_by(&subject()));
    lease.withdraw(2);
    assert!(!lease.renewable_by("device", "session", 3));
    assert!(lease.pending());
    lease.withdraw(3);
    assert!(lease.pending());
}
#[test]
fn cleanup_outlives_the_conservative_client_expiry() {
    let mut lease = lease();
    let provider_delay = 500;
    assert!(lease.issued(grant(), true, provider_delay));
    assert_eq!(lease.usable_until, LEASE_LIFETIME_MS);
    assert_eq!(lease.expires_at, LEASE_LIFETIME_MS + provider_delay);
    lease.withdraw(lease.usable_until);
    lease.settle_expiry(lease.usable_until);
    assert!(lease.pending());
    assert!(lease.live());
    assert_eq!(lease.status(), "revocation_pending");
    lease.settle_expiry(lease.expires_at);
    assert_eq!(lease.status(), "expired");
}

#[test]
fn publication_requires_persisted_key_and_matching_secret() {
    let mut state = Provisioning::Intent {
        attempt: "attempt".into(),
    };
    assert!(!state.publish("attempt", "key"));
    assert!(!state.created("other", "key"));
    assert!(state.created("attempt", "key"));
    assert!(!state.created("attempt", "another"));
    assert!(!state.publish("attempt", "another"));
    assert!(state.publish("attempt", "key"));
    assert!(state.published());
    assert!(state.publish("attempt", "key"));
    assert!(!state.created("attempt", "replacement"));
}
