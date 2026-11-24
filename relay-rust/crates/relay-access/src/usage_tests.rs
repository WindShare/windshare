use super::*;

fn sample_batch(relay_budget: Option<MonthlyBudget>) -> Batch {
    Batch {
        id: [1; 16],
        node: [2; 32],
        issuer: [3; 32],
        created_at: crate::entitlement::DAY_SECONDS * 2,
        records: Vec::new(),
        resident_accounts: Vec::new(),
        load: Load {
            relay_budget,
            applied_policy_version: 0,
            resident_shares: 0,
            active_sessions: 0,
            wire_bytes: 0,
            forwarded_bytes: 0,
            admission_refusals: 0,
        },
    }
}

#[test]
fn monthly_budget_validation_enforces_reset_day_and_storage_range() {
    for reset_day_utc in [
        MonthlyBudget::MIN_RESET_DAY_UTC,
        MonthlyBudget::MAX_RESET_DAY_UTC,
    ] {
        for bytes in [0, MonthlyBudget::MAX_BYTES] {
            let budget = MonthlyBudget {
                bytes,
                reset_day_utc,
            };
            assert_eq!(budget.validate(), Ok(()));
            assert!(budget.valid());
        }
    }
    for reset_day_utc in [0, MonthlyBudget::MAX_RESET_DAY_UTC + 1, u8::MAX] {
        let budget = MonthlyBudget {
            bytes: 1024,
            reset_day_utc,
        };
        assert_eq!(budget.validate(), Err(BudgetError::ResetDayOutOfRange));
        assert!(!budget.valid());
    }
    let oversized = MonthlyBudget {
        bytes: MonthlyBudget::MAX_BYTES + 1,
        reset_day_utc: 1,
    };
    assert_eq!(oversized.validate(), Err(BudgetError::BytesOutOfRange));
    assert!(!oversized.valid());
}

#[test]
fn batch_validation_checks_optional_relay_budget() {
    assert!(sample_batch(None).valid());
    assert!(
        sample_batch(Some(MonthlyBudget {
            bytes: MonthlyBudget::MAX_BYTES,
            reset_day_utc: MonthlyBudget::MAX_RESET_DAY_UTC,
        }))
        .valid()
    );
    assert!(
        !sample_batch(Some(MonthlyBudget {
            bytes: 1024,
            reset_day_utc: 0,
        }))
        .valid()
    );
    assert!(
        !sample_batch(Some(MonthlyBudget {
            bytes: MonthlyBudget::MAX_BYTES + 1,
            reset_day_utc: 15,
        }))
        .valid()
    );
}
