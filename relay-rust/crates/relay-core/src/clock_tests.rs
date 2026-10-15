use core::time::Duration;

use super::{UnixMillis, UnixSeconds};

#[test]
fn whole_seconds_round_up_only_past_their_start() {
    assert_eq!(UnixMillis(7_000).ceil_seconds(), UnixSeconds(7));
    assert_eq!(UnixMillis(7_001).ceil_seconds(), UnixSeconds(8));
    assert_eq!(UnixSeconds(8).start(), UnixMillis(8_000));
}

#[test]
fn overflowing_instants_saturate_instead_of_wrapping_into_the_past() {
    let latest = UnixMillis(u64::MAX - 1);
    assert_eq!(
        latest.saturating_add(Duration::from_millis(5)),
        UnixMillis(u64::MAX)
    );
    assert_eq!(latest.saturating_add(Duration::MAX), UnixMillis(u64::MAX));
    assert_eq!(UnixSeconds(u64::MAX).start(), UnixMillis(u64::MAX));
}
