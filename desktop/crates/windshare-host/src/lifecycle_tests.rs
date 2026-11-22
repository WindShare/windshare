use crate::{Model, Operation, OperationState, RelayAvailability, RelayStatus, Setup, Share};

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
    let cases = [
        (
            "awaiting_configuration",
            serde_json::json!({
                "operation_id": "selection", "state": "awaiting_configuration",
                "blocking": { "reason": "configuration_required", "recovery": "configure" }
            }),
            true,
        ),
        (
            "awaiting_network",
            serde_json::json!({
                "operation_id": "selection", "state": "awaiting_network",
                "blocking": { "reason": "directory_unavailable", "recovery": "retry" }
            }),
            true,
        ),
        (
            "awaiting_authorization",
            serde_json::json!({
                "operation_id": "selection", "state": "awaiting_authorization",
                "authorization": { "kind": "official" }
            }),
            true,
        ),
        (
            "admitting",
            serde_json::json!({ "operation_id": "selection", "state": "admitting" }),
            true,
        ),
        (
            "preparing",
            serde_json::json!({
                "operation_id": "selection", "state": "preparing", "task_id": "engine:1"
            }),
            true,
        ),
        (
            "sharing",
            serde_json::json!({
                "operation_id": "selection", "state": "sharing", "task_id": "engine:1",
                "url": "https://example.test/s/1#key"
            }),
            true,
        ),
        (
            "stopping",
            serde_json::json!({ "operation_id": "selection", "state": "stopping" }),
            true,
        ),
        (
            "finished",
            serde_json::json!({
                "operation_id": "selection", "state": "finished",
                "result": { "outcome": "stopped", "stop_reason": "share_stopped" }
            }),
            false,
        ),
    ];
    for (state, payload, unfinished) in cases {
        let share: Share = serde_json::from_value(payload).unwrap();
        assert_eq!(!share.is_finished(), unfinished, "{state}");
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
    let mut availability = RelayAvailability {
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
