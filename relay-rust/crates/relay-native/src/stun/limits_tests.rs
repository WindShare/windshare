use super::*;

fn limits(total: u32, source: u32, sources: usize) -> Limits {
    Limits {
        requests_per_second: total.try_into().unwrap(),
        source_requests_per_second: source.try_into().unwrap(),
        max_sources: sources.try_into().unwrap(),
    }
}

#[test]
fn per_ip_and_total_limits_reset_only_at_the_window_boundary() {
    let now = Instant::now();
    let a = "192.0.2.1".parse().unwrap();
    let b = "2001:db8::1".parse().unwrap();
    let mut limiter = Limiter::new(limits(3, 2, 2), now);
    assert_eq!(limiter.admit(a, now), Ok(()));
    assert_eq!(
        limiter.admit("::ffff:192.0.2.1".parse().unwrap(), now),
        Ok(())
    );
    assert_eq!(limiter.admit(a, now), Err(Refusal::SourceRate));
    assert_eq!(limiter.admit(b, now), Ok(()));
    assert_eq!(limiter.admit(b, now - WINDOW), Err(Refusal::ListenerRate));
    assert_eq!(
        limiter.admit(b, now + WINDOW - Duration::from_nanos(1)),
        Err(Refusal::ListenerRate)
    );
    assert_eq!(limiter.admit(b, now + WINDOW), Ok(()));
    assert_eq!(limiter.sources.len(), 1);
}

#[test]
fn source_capacity_cannot_be_evicted_to_bypass_a_spent_budget() {
    let now = Instant::now();
    let a = "192.0.2.1".parse().unwrap();
    let b = "192.0.2.2".parse().unwrap();
    let mut limiter = Limiter::new(limits(10, 2, 1), now);
    assert_eq!(limiter.admit(a, now), Ok(()));
    for _ in 0..10 {
        assert_eq!(limiter.admit(b, now), Err(Refusal::SourceCapacity));
    }
    assert_eq!(limiter.admit(a, now), Ok(()));
    assert_eq!(limiter.admit(a, now), Err(Refusal::SourceRate));
    assert_eq!(limiter.sources.len(), 1);
    assert_eq!(limiter.admitted, 2);
    assert_eq!(limiter.admit(b, now + WINDOW), Ok(()));
}
