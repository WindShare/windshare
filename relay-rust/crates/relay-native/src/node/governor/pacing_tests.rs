use super::{CATCH_UP_WINDOW, MAX_FRAME, Pacing};
use std::time::Duration;
use tokio::time::Instant;

const EXAMPLE_RATE: u64 = 12_500_000;

#[test]
fn a_backlogged_flow_keeps_its_configured_rate_across_delayed_wakes() {
    for rate in [EXAMPLE_RATE, EXAMPLE_RATE * 2] {
        let now = Instant::now();
        let mut pacing = Pacing::default();
        assert!(pacing.configure(Some(rate), now));
        assert_eq!(pacing.replenish(now), MAX_FRAME);
        pacing.charge(MAX_FRAME);

        let mut elapsed_ms = 0;
        let mut forwarded = 0;
        for delay_ms in [5, 5, 16, 20].into_iter().cycle().take(100) {
            elapsed_ms += delay_ms;
            let available = pacing.replenish(now + Duration::from_millis(elapsed_ms));
            let whole_frames = available / MAX_FRAME * MAX_FRAME;
            pacing.charge(whole_frames);
            forwarded += whole_frames;
        }
        let earned = rate * elapsed_ms / 1_000;
        assert_eq!(forwarded, earned / MAX_FRAME * MAX_FRAME);
    }
}

#[test]
fn idle_credit_is_bounded_even_at_the_largest_rate() {
    let rate = u64::MAX;
    let mut pacing = Pacing::default();
    let now = Instant::now();
    pacing.configure(Some(rate), now);
    let idle = now + Duration::from_secs(60);
    let catch_up =
        u128::from(rate) * CATCH_UP_WINDOW.as_nanos() / Duration::from_secs(1).as_nanos();
    let limit = MAX_FRAME + u64::try_from(catch_up).unwrap();

    assert_eq!(pacing.replenish(idle), limit);
    pacing.charge(limit);
    assert_eq!(pacing.replenish(idle), 0);
}

#[test]
fn fractional_credit_survives_wakes_and_charges() {
    let now = Instant::now();
    let mut pacing = Pacing::default();
    pacing.configure(Some(3), now);
    pacing.charge(MAX_FRAME);

    assert_eq!(pacing.replenish(now + Duration::from_millis(200)), 0);
    assert_eq!(pacing.replenish(now + Duration::from_millis(400)), 1);
    pacing.charge(1);
    assert_eq!(pacing.replenish(now + Duration::from_secs(1)), 2);
}

#[test]
fn rate_changes_clamp_credit_and_unchanged_settings_preserve_it() {
    let mut pacing = Pacing::default();
    let now = Instant::now();
    assert!(pacing.configure(Some(EXAMPLE_RATE * 2), now));
    let idle = now + Duration::from_secs(1);
    let available = pacing.replenish(idle);
    assert!(available > MAX_FRAME);
    assert!(!pacing.configure(Some(EXAMPLE_RATE * 2), idle));
    assert_eq!(pacing.replenish(idle), available);

    assert!(pacing.configure(Some(EXAMPLE_RATE), idle));
    assert_eq!(pacing.replenish(idle), MAX_FRAME);
    assert!(pacing.configure(None, idle));
    assert_eq!(pacing.replenish(idle), u64::MAX);
    pacing.charge(u64::MAX);
    assert_eq!(pacing.replenish(idle), u64::MAX);
    assert!(pacing.configure(Some(EXAMPLE_RATE), idle));
    assert_eq!(pacing.replenish(idle), MAX_FRAME);
}

#[test]
fn public_ceiling_limits_only_public_frames_with_or_without_node_capacity() {
    use super::Bandwidth;
    use windshare_relay_access::{
        Principal,
        entitlement::Class,
        governor::{Demand, Scheduler},
    };

    for node_rate in [None, Some(MAX_FRAME * 4)] {
        let now = Instant::now();
        let mut pacing = Bandwidth::default();
        pacing.node.configure(node_rate, now);
        pacing.public.configure(Some(MAX_FRAME), now);
        let mut scheduler = Scheduler::new(4, MAX_FRAME);
        let head = |principal, class, bytes| Demand {
            principal: Principal::Local([principal; 16]),
            class,
            bytes,
        };
        let mut serve = |scheduler: &mut Scheduler<u8>, now| {
            let initial = pacing.replenish(now);
            let mut remaining = initial;
            let grant = scheduler.next(|class, bytes| remaining.reserve(class, bytes));
            pacing.charge(&initial, &remaining);
            grant
        };
        assert!(scheduler.request(1, head(1, Class::Standard, MAX_FRAME)));
        assert_eq!(serve(&mut scheduler, now), Some((1, MAX_FRAME)));
        assert!(scheduler.request(1, head(1, Class::Standard, MAX_FRAME)));
        assert_eq!(serve(&mut scheduler, now), None);
        assert!(scheduler.request(2, head(2, Class::Owner, MAX_FRAME / 2)));
        if node_rate.is_some() {
            assert_eq!(serve(&mut scheduler, now), None);
        }
        let later = now + Duration::from_millis(250);
        assert_eq!(serve(&mut scheduler, later), Some((2, MAX_FRAME / 2)));
        assert_eq!(serve(&mut scheduler, later), None);
        assert_eq!(
            serve(&mut scheduler, now + Duration::from_secs(1)),
            Some((1, MAX_FRAME))
        );
    }
}
