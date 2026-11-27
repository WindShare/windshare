use super::*;
use crate::{
    Endpoint,
    node::{Settings, now},
};
use windshare_relay_access::Attribution;
use windshare_relay_core::identifiers::ShareId;

fn verify_envelope(body: &serde_json::Value, identity: &Identity) -> serde_json::Value {
    let payload = URL_SAFE_NO_PAD
        .decode(body["descriptor"].as_str().unwrap())
        .unwrap();
    let signature = URL_SAFE_NO_PAD
        .decode(body["signature"].as_str().unwrap())
        .unwrap();
    let mut signed = payload.clone();
    signed.extend_from_slice(&signature);
    let key = identity.key().verifying_key().to_bytes();
    let verified = trust::verify(Purpose::Live.domain(), &signed, &key).unwrap();
    assert_eq!(verified, payload.as_slice());
    serde_json::from_slice(&payload).unwrap()
}

#[test]
fn identify_installs_once_and_rejects_replacement() {
    let endpoint = Endpoint::parse("wss://node.example/v2/ws").unwrap();
    let first = Identity::generate().unwrap();
    let second = Identity::generate().unwrap();
    let node = Runtime::default();

    assert!(node.descriptor(&endpoint).is_err());
    node.identify(first.clone()).unwrap();
    let error = node.identify(second).unwrap_err();
    assert!(error.to_string().contains("already installed"), "{error}");

    let body = node.descriptor(&endpoint).unwrap();
    let descriptor = verify_envelope(&body, &first);
    assert_eq!(descriptor["node_id"], first.id());
}

#[tokio::test]
#[expect(
    clippy::await_holding_lock,
    reason = "intentionally held across await to prove the unbudgeted fast path skips Runtime::state"
)]
async fn services_and_unbudgeted_admission_checks_do_not_lock_runtime_state() {
    let node = Runtime::default();
    let share = ShareId::new([9; 12]).unwrap();

    let state = node.state.lock().unwrap();
    assert_eq!(
        node.services(),
        Services {
            relay: true,
            stun: true,
        }
    );
    assert!(!node.public_budget_refuses_attribution(Attribution::Anonymous));
    assert!(!node.public_budget_refuses(share, None).await.unwrap());
    drop(state);

    node.update(
        Settings {
            services: Services {
                relay: false,
                stun: false,
            },
            ..Settings::default()
        },
        now(),
    )
    .unwrap();

    let state = node.state.lock().unwrap();
    assert_eq!(
        node.services(),
        Services {
            relay: false,
            stun: false,
        }
    );
    drop(state);
}

#[tokio::test]
async fn discovery_derives_stun_from_bound_ports_and_omits_absent_listeners() {
    use crate::stun;
    let ep = Endpoint::parse("wss://[2606:4700:4700::1111]/v2/ws").unwrap();
    let identity = Identity::generate().unwrap();
    for enabled in [true, false] {
        let runtime = Runtime::default();
        runtime.identify(identity.clone()).unwrap();
        let service = stun::Service::bind_with_node(
            stun::Config {
                udp_addresses: if enabled {
                    vec!["127.0.0.1:0".parse().unwrap()]
                } else {
                    Vec::new()
                },
                admin_address: None,
                ..stun::Config::default()
            },
            runtime.clone(),
        )
        .unwrap();
        let d = verify_envelope(&runtime.descriptor(&ep).unwrap(), &identity);
        assert_eq!(d["services"]["stun"], enabled);
        let expected: Vec<_> = service
            .udp_addresses()
            .iter()
            .map(|addr| format!("stun:[2606:4700:4700::1111]:{}", addr.port()))
            .collect();
        assert_eq!(d["stun"], serde_json::json!(expected));
    }
}

#[test]
fn gossip_descriptor_caches_without_locking_state_and_invalidates_immediately() {
    use crate::Database;
    use crate::node::contribution::Policy;
    let dir = tempfile::tempdir().unwrap();
    let endpoint = Endpoint::parse("wss://cache.example/v2/ws").unwrap();
    let store = Database::open(&dir.path().join("node.sqlite"), endpoint.identity).unwrap();
    let identity = Identity::generate().unwrap();
    let node = Runtime::default();
    node.identify(identity).unwrap();
    node.configure(Settings::default(), store).unwrap();

    let t0 = now();
    let GossipDescriptor::Advertise(first) = node.gossip_descriptor_at(&endpoint, t0).unwrap()
    else {
        panic!("expected advertised gossip descriptor");
    };

    // Steady-state cache hits must neither lock Runtime::state nor re-sign the envelope.
    let state_guard = node.state.lock().unwrap();
    let GossipDescriptor::Advertise(cached) =
        node.gossip_descriptor_at(&endpoint, t0 + 10).unwrap()
    else {
        panic!("expected cached gossip descriptor");
    };
    assert_eq!(cached, first);
    drop(state_guard);

    // After GOSSIP_CACHE_TTL elapses, the node mints a fresher issued_at.
    let GossipDescriptor::Advertise(refreshed) = node
        .gossip_descriptor_at(&endpoint, t0 + i64::try_from(GOSSIP_CACHE_TTL).unwrap())
        .unwrap()
    else {
        panic!("expected refreshed gossip descriptor");
    };
    assert_ne!(refreshed.descriptor, first.descriptor);
    assert_ne!(refreshed.signature, first.signature);

    // Disabling contribution invalidates the cache immediately without waiting for TTL.
    node.set_contribution(
        Policy {
            enabled: false,
            ..Policy::default()
        },
        t0 + 10,
    )
    .unwrap();
    assert!(matches!(
        node.gossip_descriptor_at(&endpoint, t0 + 11).unwrap(),
        GossipDescriptor::OptedOut
    ));
    // OptedOut is also cached so official seeds / opted-out nodes skip Runtime::state on GET.
    let state_guard = node.state.lock().unwrap();
    assert!(matches!(
        node.gossip_descriptor_at(&endpoint, t0 + 12).unwrap(),
        GossipDescriptor::OptedOut
    ));
    drop(state_guard);

    // Reporting STUN ports invalidates the cached envelope immediately.
    node.set_contribution(Policy::default(), t0 + 12).unwrap();
    let GossipDescriptor::Advertise(before_stun) =
        node.gossip_descriptor_at(&endpoint, t0 + 12).unwrap()
    else {
        panic!("expected re-enabled gossip descriptor");
    };
    node.report_stun_ports(std::iter::once(3478)).unwrap();
    let GossipDescriptor::Advertise(after_stun) =
        node.gossip_descriptor_at(&endpoint, t0 + 12).unwrap()
    else {
        panic!("expected updated STUN gossip descriptor");
    };
    assert_ne!(after_stun.descriptor, before_stun.descriptor);
}

#[test]
fn gossip_descriptor_cache_invalidates_on_budget_exhaustion_and_rollover_boundary() {
    use crate::Database;
    use crate::node::{WriteOrigin, budget::WriteAttempt};
    use std::task::Poll;

    let dir = tempfile::tempdir().unwrap();
    let endpoint = Endpoint::parse("wss://budget.example/v2/ws").unwrap();
    let store = Database::open(&dir.path().join("node.sqlite"), endpoint.identity).unwrap();
    let node = Runtime::default();
    node.identify(Identity::generate().unwrap()).unwrap();
    let settings = Settings {
        services: Services {
            relay: true,
            stun: false,
        },
        ..Settings::default()
    };
    node.configure(settings, store).unwrap();
    let t0 = now();
    node.set_contribution(
        crate::node::contribution::Policy {
            enabled: true,
            daily_bytes: 64,
            ..Default::default()
        },
        t0,
    )
    .unwrap();

    // Warm the cache while budget remains.
    assert!(matches!(
        node.gossip_descriptor_at(&endpoint, t0).unwrap(),
        GossipDescriptor::Advertise(_)
    ));

    // Exhausting the daily public budget must immediately switch gossip to ObserverOnly.
    let origin = WriteOrigin::test_public();
    node.refill_budgets(&origin, 64, t0).unwrap();
    let WriteAttempt::Socket(Poll::Ready(Ok(64))) = node
        .write_egress(node.epoch(), &origin, 64, t0, |len| Poll::Ready(Ok(len)))
        .unwrap()
    else {
        panic!("expected 64-byte write");
    };
    assert!(matches!(
        node.gossip_descriptor_at(&endpoint, t0 + 1).unwrap(),
        GossipDescriptor::ObserverOnly
    ));

    // Crossing the UTC rollover boundary must not serve the stale exhausted ObserverOnly state.
    let next_day = node.next_rollover.load(Ordering::Acquire);
    assert!(matches!(
        node.gossip_descriptor_at(&endpoint, next_day).unwrap(),
        GossipDescriptor::Advertise(_)
    ));
}
