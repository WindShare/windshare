use super::*;

const KEY: AccountKey = ([1; 32], [2; 16]);
const MIDNIGHT: u64 = 22 * DAY_SECONDS;

fn quota(version: u64, day: u64, valid_until: u64) -> Quota {
    Quota {
        day,
        version,
        valid_until,
        remaining_bytes: Some(10),
    }
}

#[test]
fn quota_expires_before_its_version_and_previous_days_cannot_be_revived() {
    let mut quotas = Quotas::default();
    let now = MIDNIGHT - 200;
    let old = quota(1, 21, now + 100);
    let newer = quota(2, 21, now + 50);
    assert!(quotas.advance(KEY, old, now));
    assert!(quotas.advance(KEY, newer, now));
    assert_eq!(quotas.allowance(KEY, now), Allowance::Available);

    assert_eq!(quotas.retire_before(21, now + 50), 1);
    assert_eq!(quotas.accounts[&KEY].remaining_bytes, None);
    assert_eq!(quotas.accounts[&KEY].valid_until, newer.valid_until);
    assert_eq!(quotas.allowance(KEY, now + 50), Allowance::Stale);
    assert!(!quotas.advance(KEY, old, now + 50));
    assert!(!quotas.advance(KEY, newer, now + 50));

    let renewed = quota(2, 21, now + 110);
    assert!(quotas.advance(KEY, renewed, now + 50));
    assert_eq!(quotas.allowance(KEY, now + 50), Allowance::Available);
    assert!(!quotas.advance(KEY, newer, now + 50));

    assert_eq!(quotas.retire_before(21, now + 110), 1);
    assert_eq!(quotas.allowance(KEY, now + 110), Allowance::Stale);
    assert_eq!(quotas.retire_before(22, MIDNIGHT), 1);
    assert!(quotas.accounts.is_empty());
    assert!(quotas.deadlines.is_empty());
    assert!(!quotas.advance(KEY, old, now));
}

#[test]
fn next_day_quota_received_inside_clock_skew_is_available_at_midnight() {
    let mut quotas = Quotas::default();
    assert!(quotas.advance(KEY, quota(9, 21, MIDNIGHT), MIDNIGHT - 1));
    assert!(quotas.advance(KEY, quota(1, 22, MIDNIGHT + 30), MIDNIGHT - 1));
    assert_eq!(quotas.allowance(KEY, MIDNIGHT - 1), Allowance::Stale);
    assert_eq!(quotas.retire_before(22, MIDNIGHT), 0);
    assert_eq!(quotas.allowance(KEY, MIDNIGHT), Allowance::Available);
}
