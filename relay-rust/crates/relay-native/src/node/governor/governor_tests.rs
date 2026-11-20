use super::*;
use crate::node::access::{Policy, Service};
use windshare_relay_access::entitlement::{DAY_SECONDS, ENTITLEMENT_LIFETIME, Entitlement};

fn add_lane(
    state: &mut State,
    id: u64,
    session: Key,
    attribution: Attribution,
    demand: Option<u64>,
) {
    let wake = Arc::new(Notify::new());
    state.lanes.insert(
        id,
        Lane {
            session,
            attribution,
            wake,
            demand,
            granted: false,
        },
    );
    *state.references.entry(session).or_default() += 1;
}

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
    add_lane(&mut state, 1, key, attribution, Some(1_000));
    add_lane(&mut state, 2, key, attribution, None);
    assert!(state.scheduler.request(
        key,
        Demand {
            principal: attribution.principal(key.0),
            class: Class::Standard,
            bytes: 1_000,
        }
    ));
    state.classified = now.saturating_sub(1);
    assert_eq!(state.refresh_classes(&access), NO_CHANGES);

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
    state.classified = now.saturating_sub(1);
    let reclassified = state.refresh_classes(&access);
    assert_eq!(
        reclassified,
        vec![(attribution.principal(key.0), Class::Standard, Class::Lowest)]
    );
    assert_eq!(state.refresh_classes(&access), NO_CHANGES);
    drop(state);
    log_reclassifications(reclassified);
}

#[tokio::test(start_paused = true)]
async fn paced_governor_shares_public_capacity_weighted_while_keeping_owner_strict() {
    const FRAME: u64 = MAX_FRAME;
    let access = Service::default();
    let owner_principal = [7; 16];
    access
        .configure(Policy {
            owner_principals: vec![owner_principal],
            ..Policy::default()
        })
        .unwrap();
    let owner_attr = Attribution::Credential {
        principal: owner_principal,
        credential: [7; 16],
    };

    let now = super::super::now().unsigned_abs();
    let issuer = [8; 32];
    let standard_account = [1; 16];
    let standard_attr = Attribution::Account {
        issuer,
        account: standard_account,
        credential: [1; 16],
    };
    assert!(access.refresh(
        issuer,
        standard_account,
        Entitlement {
            version: 1,
            day: now / DAY_SECONDS,
            valid_until: now + ENTITLEMENT_LIFETIME,
            remaining_bytes: 100 * FRAME,
        },
        now,
    ));
    let lowest_attr = Attribution::Anonymous;

    let governor = Governor::new(access.clone());
    governor.publish_capacity(Some(FRAME), Some(FRAME), || {});

    let owner_key = Key([3; 12], [3; 16], 3);
    let standard_key = Key([1; 12], [1; 16], 1);
    let lowest_key = Key([2; 12], [2; 16], 2);
    {
        let mut state = governor.state.lock().unwrap();
        add_lane(&mut state, 1, standard_key, standard_attr, None);
        add_lane(&mut state, 2, lowest_key, lowest_attr, None);
        add_lane(&mut state, 3, owner_key, owner_attr, None);
    }

    let step = |active: &[(u64, Key)]| {
        let mut state = governor.state.lock().unwrap();
        for &(id, session) in active {
            let lane = state.lanes.get_mut(&id).unwrap();
            if lane.demand.is_none() {
                lane.demand = Some(FRAME);
                lane.granted = false;
                state.queues.entry(session).or_default().push_back(id);
                let demand = state.demand(id, &access);
                assert!(state.scheduler.request(session, demand));
            }
        }
        let mut granted_ids = Vec::new();
        state.grant_pending(&access, |id, _| granted_ids.push(id));
        for &id in &granted_ids {
            let lane = state.lanes.get_mut(&id).unwrap();
            lane.demand = None;
            lane.granted = false;
        }
        drop(state);
        granted_ids
    };

    // While Owner saturates node capacity, public classes receive zero grants.
    let mut owner_grants = 0_usize;
    let mut public_during_owner = 0_usize;
    for _ in 0..10 {
        for id in step(&[(1, standard_key), (2, lowest_key), (3, owner_key)]) {
            if id == 3 {
                owner_grants += 1;
            } else {
                public_during_owner += 1;
            }
        }
        tokio::time::advance(Duration::from_secs(1)).await;
    }
    assert!(owner_grants >= 10);
    assert_eq!(public_during_owner, 0);

    // Once Owner goes idle, Standard and Lowest share public capacity at 19:1.
    let mut standard_grants = 0_usize;
    let mut lowest_grants = 0_usize;
    for _ in 0..40 {
        for id in step(&[(1, standard_key), (2, lowest_key)]) {
            if id == 1 {
                standard_grants += 1;
            } else if id == 2 {
                lowest_grants += 1;
            }
        }
        tokio::time::advance(Duration::from_secs(1)).await;
    }
    assert_eq!(standard_grants, 38);
    assert_eq!(lowest_grants, 2);
}

#[tokio::test(start_paused = true)]
async fn disabling_pacing_wakes_waiting_lanes_and_switches_to_unpaced_fast_path() {
    use futures_util::FutureExt;

    let access = Service::default();
    let governor = Governor::new(access);
    governor.publish_capacity(Some(MAX_FRAME), None, || {});

    let wake = Arc::new(Notify::new());
    {
        let mut state = governor.state.lock().unwrap();
        let key = Key([1; 12], [2; 16], 1);
        state.lanes.insert(
            1,
            Lane {
                session: key,
                attribution: Attribution::Anonymous,
                wake: Arc::clone(&wake),
                demand: None,
                granted: false,
            },
        );
        *state.references.entry(key).or_default() += 1;
    }
    let lease = LaneLease {
        id: 1,
        governor: governor.clone(),
    };

    assert!(lease.try_take(MAX_FRAME));
    let _ = wake.notified().now_or_never();
    assert!(!lease.try_take(MAX_FRAME));

    // Disabling pacing immediately releases throttled lanes and bypasses queueing.
    governor.publish_capacity(None, None, || {});
    assert!(wake.notified().now_or_never().is_some());
    assert!(lease.try_take(MAX_FRAME));
    assert!(lease.try_take(MAX_FRAME));
}
