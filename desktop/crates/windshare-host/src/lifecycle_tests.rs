use crate::{Model, Operation, OperationState, RelayStatus, Setup, Share, ShareState};

#[test]
fn unfinished_work_includes_unacknowledged_creation_and_setup_cleanup() {
    let mut model = Model::default();
    assert!(!model.has_unfinished_work());
    model.operations.push(Operation {
        id: "selection".into(),
        paths: vec!["file".into()],
        state: OperationState::Submitting,
    });
    assert!(model.has_unfinished_work());
    for state in [
        "awaiting_configuration",
        "awaiting_network",
        "awaiting_authorization",
        "admitting",
        "preparing",
        "sharing",
        "stopping",
        "finished",
    ] {
        let share: Share = serde_json::from_value(serde_json::json!({
            "operation_id": "selection", "state": state, "startup": "pending",
            "blocking": { "reason": "configuration_required", "recovery": "configure" }
        }))
        .unwrap();
        let unfinished = share.state != ShareState::Finished;
        model.operations[0].state = OperationState::Accepted(Box::new(share));
        assert_eq!(model.has_unfinished_work(), unfinished, "{state}");
    }
    for state in ["running", "cancelling", "cancel_failed"] {
        model.setup = serde_json::from_value(serde_json::json!({
            "state": state, "id": "setup:1", "frontend_saved": true,
            "error": { "code": "setup_cancel_failed", "message": "offline" }
        }))
        .unwrap();
        assert!(model.has_unfinished_work(), "{state}");
        assert!(model.setup.context().unwrap().frontend_saved);
    }
    model.setup = Setup::Idle;
    assert!(!model.has_unfinished_work());
}

#[test]
fn terminal_relay_admission_is_distinct_from_active_recovery() {
    let mut availability = super::model::RelayAvailability {
        available: 1,
        total: 2,
        terminal: 0,
        ever_ready: true,
    };
    assert_eq!(availability.status(), RelayStatus::Available);
    availability.available = 0;
    assert_eq!(availability.status(), RelayStatus::Recovering);
    availability.terminal = 2;
    assert_eq!(availability.status(), RelayStatus::Unavailable);
}
