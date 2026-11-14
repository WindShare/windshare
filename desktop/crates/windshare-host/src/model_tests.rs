use super::*;

#[test]
fn blocking_contract_preserves_reason_recovery_and_deadline() {
    let source = serde_json::json!({
        "operation_id": "selection", "state": "awaiting_network", "startup": "pending",
        "blocking": { "reason": "directory_unavailable", "recovery": "retry", "retry_at_ms": 12345 }
    });
    let share: Share = serde_json::from_value(source).unwrap();
    assert_eq!(
        share.state,
        ShareState::AwaitingNetwork {
            blocking: CreationBlock {
                reason: BlockingReason::DirectoryUnavailable,
                recovery: RecoveryAction::Retry,
                retry_at_ms: Some(12345),
            },
        }
    );
    let invalid = serde_json::json!({
        "operation_id": "selection", "state": "awaiting_network", "startup": "pending"
    });
    assert!(serde_json::from_value::<Share>(invalid).is_err());
    let setup: Setup = serde_json::from_value(serde_json::json!({
        "state": "finished", "id": "setup:1"
    }))
    .unwrap();
    assert!(matches!(setup, Setup::Finished { context } if context.operation_id.is_empty()));
}

fn snapshot(revision: u64, shares: Vec<Share>) -> Snapshot {
    Snapshot {
        session_id: "first".into(),
        revision,
        shares,
        setup: Setup::Idle,
    }
}

fn accepted(id: &str, state: ShareState) -> Share {
    Share {
        operation_id: id.into(),
        task_id: format!("engine:{id}"),
        state,
        startup: Startup::Ready,
        url: "https://example.test/share#key".into(),
        created_at_ms: 0,
        selection: None,
        result: None,
        error: None,
        relay: None,
        authorization: None,
    }
}

#[test]
fn snapshots_cannot_move_backwards_or_cross_host_sessions() {
    let mut model = Model::default();
    model.reconcile(snapshot(5, vec![accepted("one", ShareState::Stopping)]));
    model.reconcile(snapshot(3, vec![accepted("one", ShareState::Sharing)]));
    model.reconcile(Snapshot {
        session_id: "retired".into(),
        revision: 100,
        shares: vec![],
        setup: Setup::Idle,
    });
    assert_eq!(model.revision, 5);
    assert_eq!(
        model.operations[0].share().unwrap().state,
        ShareState::Stopping
    );
    model.reconcile(snapshot(6, vec![accepted("one", ShareState::Finished)]));
    assert_eq!(
        model.operations[0].share().unwrap().state,
        ShareState::Finished
    );
}

#[test]
fn reconnect_keeps_selections_and_creates_not_yet_admitted() {
    let mut model = Model::default();
    for id in ["one", "two"] {
        model.operations.push(Operation {
            id: id.into(),
            paths: vec![format!("{id}.txt")],
            state: OperationState::Submitting,
        });
    }
    model.reconcile(snapshot(1, vec![]));
    model.reconcile(snapshot(2, vec![accepted("one", ShareState::Sharing)]));
    assert_eq!(model.operations.len(), 2);
    assert_eq!(model.operations[0].paths, ["one.txt"]);
    assert!(matches!(
        model.operations[1].state,
        OperationState::Submitting
    ));
    model.reconcile(snapshot(
        3,
        vec![
            accepted("one", ShareState::Sharing),
            accepted("two", ShareState::Sharing),
        ],
    ));
    assert_eq!(model.operations.len(), 2);
    assert!(
        model
            .operations
            .iter()
            .all(|operation| operation.share().is_some())
    );
}
