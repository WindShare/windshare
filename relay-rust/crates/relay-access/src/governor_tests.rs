use super::*;

fn request(principal: u8, class: Class, bytes: u64) -> Demand {
    Demand {
        principal: Principal::Local([principal; 16]),
        class,
        bytes,
    }
}

#[test]
fn extra_sessions_do_not_multiply_a_principals_bandwidth() {
    let mut scheduler = Scheduler::new(100, 1000);
    for key in 0..11 {
        assert!(scheduler.request(
            key,
            request(if key == 0 { 1 } else { 2 }, Class::Standard, 100)
        ));
    }
    let mut served = [0; 11];
    for _ in 0..200 {
        let (key, bytes) = scheduler.next(|_, bytes| bytes <= 1000).unwrap();
        served[key] += bytes;
        assert!(scheduler.request(
            key,
            request(if key == 0 { 1 } else { 2 }, Class::Standard, 100)
        ));
    }
    assert_eq!(served[0], 10_000);
    assert_eq!(served[1..].iter().sum::<u64>(), 10_000);
    assert!(served[1..].iter().all(|bytes| *bytes == 1000));
}

#[test]
fn fairness_counts_bytes_and_owner_preempts_without_reservation() {
    let mut scheduler = Scheduler::new(10, 1000);
    assert!(scheduler.request(0, request(1, Class::Standard, 1000)));
    assert!(scheduler.request(1, request(2, Class::Standard, 100)));
    let mut served = [0; 2];
    for _ in 0..110 {
        let (key, bytes) = scheduler.next(|_, bytes| bytes <= 1000).unwrap();
        served[key] += bytes;
        assert!(scheduler.request(
            key,
            request(u8::try_from(key + 1).unwrap(), Class::Standard, bytes)
        ));
    }
    assert_eq!(served, [10_000, 10_000]);
    assert!(scheduler.request(2, request(3, Class::Owner, 10)));
    assert_eq!(scheduler.next(|_, bytes| bytes <= 1000), Some((2, 10)));
    let (key, bytes) = scheduler.next(|_, bytes| bytes <= 1000).unwrap();
    assert!(scheduler.request(
        key,
        request(u8::try_from(key + 1).unwrap(), Class::Standard, bytes)
    ));
    assert_eq!(
        scheduler.reclassify(Principal::Local([2; 16]), Class::Lowest),
        Some(Class::Standard)
    );
    assert_eq!(
        scheduler.reclassify(Principal::Local([2; 16]), Class::Lowest),
        None
    );
    assert_eq!(scheduler.next(|_, bytes| bytes <= 1000), Some((0, 1000)));
}

#[test]
fn queues_bound_requests_and_wait_for_large_frames() {
    let mut scheduler = Scheduler::new(2, 1000);
    assert!(scheduler.request(1, request(1, Class::Owner, 1000)));
    assert!(!scheduler.request(1, request(1, Class::Owner, 10)));
    assert!(scheduler.request(2, request(2, Class::Lowest, 1)));
    assert!(!scheduler.request(3, request(3, Class::Standard, 10)));
    assert_eq!(scheduler.next(|_, bytes| bytes <= 999), None);
    scheduler.cancel(1);
    assert_eq!(scheduler.next(|_, bytes| bytes <= 1), Some((2, 1)));
    scheduler.cancel(2);
    assert_eq!(scheduler.pending(), 0);
    assert!(!scheduler.request(3, request(3, Class::Standard, 1001)));
}

#[test]
fn public_classes_converge_to_weighted_share_without_starving_lowest() {
    const FRAME: u64 = 65_536;
    const ROUNDS: usize = 200;
    let mut scheduler = Scheduler::new(4, FRAME);
    assert!(scheduler.request(0, request(1, Class::Standard, FRAME)));
    assert!(scheduler.request(1, request(2, Class::Lowest, FRAME)));

    let mut served = [0_u64; 2];
    for _ in 0..ROUNDS {
        let (key, bytes) = scheduler.next(|_, _| true).unwrap();
        served[key] += bytes;
        let class = if key == 0 {
            Class::Standard
        } else {
            Class::Lowest
        };
        assert!(scheduler.request(key, request(u8::try_from(key + 1).unwrap(), class, FRAME)));
    }
    assert_eq!(served[0], 190 * FRAME);
    assert_eq!(served[1], 10 * FRAME);
}

#[test]
fn many_lowest_principals_cannot_dilute_standard_class_share() {
    const FRAME: u64 = 16_384;
    const LOWEST_PRINCIPALS: usize = 50;
    const ROUNDS: usize = 400;
    let mut scheduler = Scheduler::new(LOWEST_PRINCIPALS + 1, FRAME);
    assert!(scheduler.request(0, request(1, Class::Standard, FRAME)));
    for key in 1..=LOWEST_PRINCIPALS {
        let principal = u8::try_from(key + 1).unwrap();
        assert!(scheduler.request(key, request(principal, Class::Lowest, FRAME)));
    }

    let mut standard_bytes = 0_u64;
    let mut lowest_bytes = 0_u64;
    for _ in 0..ROUNDS {
        let (key, bytes) = scheduler.next(|_, _| true).unwrap();
        if key == 0 {
            standard_bytes += bytes;
            assert!(scheduler.request(0, request(1, Class::Standard, FRAME)));
        } else {
            lowest_bytes += bytes;
            let principal = u8::try_from(key + 1).unwrap();
            assert!(scheduler.request(key, request(principal, Class::Lowest, FRAME)));
        }
    }
    assert_eq!(standard_bytes, 380 * FRAME);
    assert_eq!(lowest_bytes, 20 * FRAME);
}

#[test]
fn small_lowest_signaling_frames_pass_promptly_under_standard_bulk_saturation() {
    const BULK: u64 = 65_536;
    const SIGNAL: u64 = 2_048;
    let mut scheduler = Scheduler::new(4, BULK);
    assert!(scheduler.request(0, request(1, Class::Standard, BULK)));
    for _ in 0..50 {
        assert_eq!(scheduler.next(|_, _| true), Some((0, BULK)));
        assert!(scheduler.request(0, request(1, Class::Standard, BULK)));
    }

    // A 2 KiB signaling/handshake frame costs 38 KiB in virtual time (< 64 KiB bulk),
    // so it passes ahead of the next bulk frame without letting bulk Lowest starve Standard.
    assert!(scheduler.request(1, request(2, Class::Lowest, SIGNAL)));
    assert_eq!(scheduler.next(|_, _| true), Some((1, SIGNAL)));
    assert_eq!(scheduler.next(|_, _| true), Some((0, BULK)));
}

#[test]
fn owner_strictly_preempts_public_classes_without_distorting_their_ratio() {
    const FRAME: u64 = 1_000;
    let mut scheduler = Scheduler::new(4, FRAME);
    assert!(scheduler.request(0, request(1, Class::Owner, FRAME)));
    assert!(scheduler.request(1, request(2, Class::Standard, FRAME)));
    assert!(scheduler.request(2, request(3, Class::Lowest, FRAME)));

    for _ in 0..50 {
        assert_eq!(scheduler.next(|_, _| true), Some((0, FRAME)));
        assert!(scheduler.request(0, request(1, Class::Owner, FRAME)));
    }
    scheduler.cancel(0);

    let mut standard = 0;
    let mut lowest = 0;
    for _ in 0..40 {
        let (key, bytes) = scheduler.next(|_, _| true).unwrap();
        if key == 1 {
            standard += bytes;
            assert!(scheduler.request(1, request(2, Class::Standard, FRAME)));
        } else {
            lowest += bytes;
            assert!(scheduler.request(2, request(3, Class::Lowest, FRAME)));
        }
    }
    assert_eq!(standard, 38 * FRAME);
    assert_eq!(lowest, 2 * FRAME);
}

#[test]
fn idle_public_classes_are_work_conserving_and_cannot_bank_credit() {
    const FRAME: u64 = 1_000;
    let mut scheduler = Scheduler::new(4, FRAME);
    assert!(scheduler.request(1, request(2, Class::Lowest, FRAME)));
    for _ in 0..30 {
        assert_eq!(scheduler.next(|_, _| true), Some((1, FRAME)));
        assert!(scheduler.request(1, request(2, Class::Lowest, FRAME)));
    }

    // Standard arriving after idle Lowest immediately preempts.
    assert!(scheduler.request(0, request(1, Class::Standard, FRAME)));
    assert_eq!(scheduler.next(|_, _| true), Some((0, FRAME)));

    // Drain Lowest while Standard runs alone; returning Lowest cannot burst ahead.
    scheduler.cancel(1);
    assert!(scheduler.request(0, request(1, Class::Standard, FRAME)));
    for _ in 0..100 {
        assert_eq!(scheduler.next(|_, _| true), Some((0, FRAME)));
        assert!(scheduler.request(0, request(1, Class::Standard, FRAME)));
    }
    assert!(scheduler.request(1, request(2, Class::Lowest, FRAME)));
    for _ in 0..19 {
        assert_eq!(scheduler.next(|_, _| true), Some((0, FRAME)));
        assert!(scheduler.request(0, request(1, Class::Standard, FRAME)));
    }
    assert_eq!(scheduler.next(|_, _| true), Some((1, FRAME)));
}

#[test]
fn weighted_ratio_survives_batch_drains_and_extra_sessions_on_waiting_lowest() {
    const FRAME: u64 = 1_000;
    let mut scheduler = Scheduler::new(6, FRAME);
    assert!(scheduler.request(0, request(1, Class::Standard, FRAME)));
    assert!(scheduler.request(1, request(2, Class::Lowest, FRAME)));

    // Both single-session classes granted before either actor re-enqueues, with Lowest waking first.
    assert_eq!(scheduler.next(|_, _| true), Some((0, FRAME)));
    assert_eq!(scheduler.next(|_, _| true), Some((1, FRAME)));
    assert!(scheduler.request(1, request(2, Class::Lowest, FRAME)));
    assert!(scheduler.request(0, request(1, Class::Standard, FRAME)));

    // After the 1:1 batch drain, Standard receives 37 frames (bringing total Standard
    // to 38 before Lowest's 2nd frame, exact 19:1). Adding a second session to the
    // waiting Lowest principal right before its turn must not reset Lowest's served
    // position up to public_clock.
    for _ in 0..36 {
        assert_eq!(scheduler.next(|_, _| true), Some((0, FRAME)));
        assert!(scheduler.request(0, request(1, Class::Standard, FRAME)));
    }
    assert!(scheduler.request(2, request(2, Class::Lowest, FRAME)));
    assert_eq!(scheduler.next(|_, _| true), Some((0, FRAME)));
    assert!(scheduler.request(0, request(1, Class::Standard, FRAME)));
    let (key, bytes) = scheduler.next(|_, _| true).unwrap();
    assert!(key == 1 || key == 2);
    assert_eq!(bytes, FRAME);
}
