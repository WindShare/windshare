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
