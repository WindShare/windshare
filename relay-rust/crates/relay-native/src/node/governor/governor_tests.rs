use super::*;
use crate::node::access::Service;
use windshare_relay_access::entitlement::{DAY_SECONDS, ENTITLEMENT_LIFETIME, Entitlement};

#[test]
fn governor_reclassification_updates_class_once_per_transition() {
    const NO_CHANGES: &[(Principal, Class, Class)] = &[];
    let access = Service::default();
    let now = super::super::now().unsigned_abs();
    let issuer = [8; 32];
    let account = [9; 16];
    let attribution = Attribution::Account {
        issuer,
        account,
        credential: [4; 16],
    };
    let day = now / DAY_SECONDS;
    assert!(access.refresh(
        issuer,
        account,
        Entitlement {
            version: 1,
            day,
            valid_until: now + ENTITLEMENT_LIFETIME,
            remaining_bytes: 1_024,
        },
        now,
    ));

    let governor = Governor::new(access.clone());
    let mut state = governor.state.lock().unwrap();
    let key = Key([9; 12], [2; 16], 1);
    let id = state
        .register(key, attribution, Arc::default(), Arc::default())
        .unwrap();
    state
        .register(key, attribution, Arc::default(), Arc::default())
        .unwrap();
    state.refresh_classes(&access, now);
    state.offer(id, std::iter::once(1_000), &access);
    assert_eq!(state.refresh_classes(&access, now), NO_CHANGES);

    assert!(access.refresh(
        issuer,
        account,
        Entitlement {
            version: 2,
            day,
            valid_until: now + ENTITLEMENT_LIFETIME,
            remaining_bytes: 0,
        },
        now,
    ));
    let reclassified = state.refresh_classes(&access, now + 1);
    assert_eq!(
        reclassified,
        vec![(attribution.principal(key.0), Class::Standard, Class::Lowest)]
    );
    assert_eq!(state.refresh_classes(&access, now + 1), NO_CHANGES);
    drop(state);
    log_reclassifications(reclassified);
}
