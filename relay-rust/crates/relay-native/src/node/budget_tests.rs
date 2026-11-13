use super::*;
use crate::Endpoint;

fn timestamp(raw: &str) -> i64 {
    chrono::DateTime::parse_from_rfc3339(raw)
        .unwrap()
        .timestamp()
}

#[test]
fn billing_period_is_utc_and_never_uses_restart_time() {
    for (now, expected) in [
        ("2026-01-01T00:00:00Z", "2025-12-15T00:00:00Z"),
        ("2026-01-14T23:59:59Z", "2025-12-15T00:00:00Z"),
        ("2026-01-15T00:00:00Z", "2026-01-15T00:00:00Z"),
    ] {
        assert_eq!(
            period_start(timestamp(now), 15).unwrap(),
            timestamp(expected)
        );
    }
}

#[test]
fn ledger_survives_restart_clock_rollback_and_service_changes() {
    let directory = tempfile::tempdir().unwrap();
    let endpoint = Endpoint::parse("wss://relay.example/v2/ws").unwrap();
    let path = directory.path().join("node.sqlite");
    let config = MonthlyBudget {
        bytes: 100,
        reset_day_utc: 15,
    };
    let now = timestamp("2026-11-16T00:00:00Z");
    {
        let store = Database::open(&path, endpoint.identity).unwrap();
        let mut budget = Budget::new(store).unwrap();
        budget.update(config, now).unwrap();
        assert_eq!(budget.reserve(100, 60).unwrap(), 60);
        assert_eq!(budget.reserve(100, 60).unwrap(), 40);
        assert_eq!(budget.reserve(100, 1).unwrap(), 0);
    }
    let store = Database::open(&path, endpoint.identity).unwrap();
    let mut budget = Budget::new(store).unwrap();
    budget.update(config, now - 86400 * 40).unwrap();
    assert_eq!(budget.reserve(100, 1).unwrap(), 0);
    assert!(
        budget
            .update(
                MonthlyBudget {
                    reset_day_utc: 1,
                    ..config
                },
                now
            )
            .is_err()
    );
    budget
        .update(config, timestamp("2026-12-15T00:00:00Z"))
        .unwrap();
    assert_eq!(budget.reserve(100, 80).unwrap(), 80);
}

#[test]
fn interrupted_reservations_fail_closed() {
    let directory = tempfile::tempdir().unwrap();
    let endpoint = Endpoint::parse("wss://relay.example/v2/ws").unwrap();
    let path = directory.path().join("node.sqlite");
    let config = MonthlyBudget {
        bytes: RESERVATION_BYTES * 3,
        reset_day_utc: 1,
    };
    let now = timestamp("2026-11-16T00:00:00Z");
    {
        let mut budget = Budget::new(Database::open(&path, endpoint.identity).unwrap()).unwrap();
        budget.update(config, now).unwrap();
        assert_eq!(budget.reserve(config.bytes, 5).unwrap(), 5);
    }
    let mut budget = Budget::new(Database::open(&path, endpoint.identity).unwrap()).unwrap();
    budget.update(config, now).unwrap();
    assert_eq!(budget.remaining(config.bytes), RESERVATION_BYTES * 2);
}
