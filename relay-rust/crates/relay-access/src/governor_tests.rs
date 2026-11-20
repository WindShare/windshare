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
    scheduler.reclassify(Principal::Local([2; 16]), Class::Lowest);
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
