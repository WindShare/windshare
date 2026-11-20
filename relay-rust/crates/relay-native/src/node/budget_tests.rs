use super::*;
use crate::{Database, Endpoint, node::MonthlyBudget};

fn timestamp(raw: &str) -> i64 {
    chrono::DateTime::parse_from_rfc3339(raw)
        .unwrap()
        .timestamp()
}

fn configure(store: &Database, budgets: &mut Budgets, settings: &Settings, now: i64) -> Result<()> {
    let records = budgets.rollover(settings, now)?;
    store.persist_budgets(records)?;
    budgets.advance(records);
    Ok(())
}

fn spend(store: &Database, budgets: &mut Budgets, limits: Limits, bytes: u64) -> u64 {
    let records = budgets.refill(limits, bytes);
    store.persist_budgets(records).unwrap();
    budgets.funded(records);
    let written = budgets.ready(limits).min(bytes);
    budgets.charge(limits, written);
    written
}

#[test]
fn billing_period_is_utc_and_never_uses_restart_time() {
    for (now, expected) in [
        ("2026-01-01T00:00:00Z", "2025-12-15T00:00:00Z"),
        ("2026-01-14T23:59:59Z", "2025-12-15T00:00:00Z"),
        ("2026-01-15T00:00:00Z", "2026-01-15T00:00:00Z"),
    ] {
        assert_eq!(
            ledger::period_start(Window::BillingMonth { reset_day: 15 }, timestamp(now)).unwrap(),
            timestamp(expected)
        );
    }
}

#[test]
fn restart_and_clock_rollback_preserve_both_ledgers_and_utc_midnight_resets_only_public() {
    let directory = tempfile::tempdir().unwrap();
    let endpoint = Endpoint::parse("wss://relay.example/v2/ws").unwrap();
    let path = directory.path().join("node.sqlite");
    let settings = Settings {
        relay_budget: Some(MonthlyBudget {
            bytes: 4_000_000,
            reset_day_utc: 15,
        }),
        contribution: crate::node::ContributionLimits {
            daily_bytes: Some(100),
            ..Default::default()
        },
        ..Settings::default()
    };
    let limits = Limits {
        monthly: Some(4_000_000),
        public_daily: Some(100),
    };
    let now = timestamp("2026-11-16T23:59:59Z");
    let open = || Database::open(&path, endpoint.identity).unwrap();
    {
        let store = open();
        let mut budgets = Budgets::restore(store.load_budgets().unwrap());
        configure(&store, &mut budgets, &settings, now).unwrap();
        assert_eq!(spend(&store, &mut budgets, limits, 60), 60);
        assert_eq!(spend(&store, &mut budgets, limits, 60), 40);
        assert_eq!(budgets.node_remaining(&settings), 3_999_900);
    }
    let store = open();
    let mut budgets = Budgets::restore(store.load_budgets().unwrap());
    configure(
        &store,
        &mut budgets,
        &settings,
        timestamp("2026-10-01T00:00:00Z"),
    )
    .unwrap();
    assert_eq!(spend(&store, &mut budgets, limits, 1), 0);
    // Only the monthly ledger had unused batch credit at process exit.
    let remaining = budgets.node_remaining(&settings);
    assert_eq!(remaining, 4_000_000 - 1024 * 1024);
    configure(&store, &mut budgets, &settings, now + 1).unwrap();
    assert_eq!(spend(&store, &mut budgets, limits, 100), 100);
    assert_eq!(budgets.node_remaining(&settings), remaining - 100);
    let mut invalid = settings.clone();
    invalid.relay_budget.as_mut().unwrap().reset_day_utc = 1;
    assert!(configure(&store, &mut budgets, &invalid, now + 1).is_err());
    configure(
        &store,
        &mut budgets,
        &settings,
        timestamp("2026-12-15T00:00:00Z"),
    )
    .unwrap();
    assert_eq!(budgets.node_remaining(&settings), 4_000_000);
    assert_eq!(budgets.public_remaining(&settings), 100);
}
