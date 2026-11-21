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

    let with_backup: Share = serde_json::from_value(serde_json::json!({
        "operation_id": "share", "state": "sharing", "startup": "ready",
        "url": "https://ws.example/id#key", "backup_url": "https://backup.example/id#key"
    }))
    .unwrap();
    assert_eq!(with_backup.backup_url, "https://backup.example/id#key");
}

fn snapshot(revision: u64, shares: Vec<Share>) -> Snapshot {
    Snapshot {
        session_id: "first".into(),
        revision,
        shares,
        setup: Setup::Idle,
        preferences: crate::RelayPreferences::default(),
        account: None,
        sender_trace: crate::SenderTrace::Off,
    }
}

fn accepted(id: &str, state: ShareState) -> Share {
    Share {
        operation_id: id.into(),
        task_id: format!("engine:{id}"),
        state,
        startup: Startup::Ready,
        url: "https://example.test/share#key".into(),
        backup_url: String::new(),
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
        preferences: crate::RelayPreferences::default(),
        account: None,
        sender_trace: crate::SenderTrace::Off,
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
            paths: vec![format!("{id}.txt").into()],
            state: OperationState::Submitting,
        });
    }
    model.reconcile(snapshot(1, vec![]));
    model.reconcile(snapshot(2, vec![accepted("one", ShareState::Sharing)]));
    assert_eq!(model.operations.len(), 2);
    assert_eq!(
        model.operations[0].paths,
        [std::path::PathBuf::from("one.txt")]
    );
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

#[test]
fn account_storage_error_deserializes_and_disables_profile_edits() {
    let view: crate::AccountView = serde_json::from_value(serde_json::json!({
        "status": {
            "active": true,
            "state": "storage_error",
            "account": "acct-1",
            "allowance": "stale",
            "remaining_bytes": 0,
            "reason": "invalid account state"
        }
    }))
    .unwrap();
    assert_eq!(view.status.state, crate::AccountState::StorageError);
    assert_eq!(view.status.reason, "invalid account state");
    assert!(!view.can_edit_profile());
}

#[test]
fn allowance_reset_local_requires_ready_non_stale_allowance() {
    let cases = [
        (
            "ready",
            "available",
            "2026-11-21 08:00 CST",
            Some("2026-11-21 08:00 CST"),
        ),
        (
            "ready",
            "spent",
            "2026-11-21 08:00 CST",
            Some("2026-11-21 08:00 CST"),
        ),
        ("ready", "stale", "2026-11-21 08:00 CST", None),
        ("ready", "available", "", None),
        (
            "refresh_required",
            "available",
            "2026-11-21 08:00 CST",
            None,
        ),
        (
            "temporarily_unavailable",
            "available",
            "2026-11-21 08:00 CST",
            None,
        ),
    ];
    for (state, allowance, reset_local, expected) in cases {
        let view: crate::AccountView = serde_json::from_value(serde_json::json!({
            "status": {
                "active": true,
                "state": state,
                "account": "acct-1",
                "allowance": allowance,
                "remaining_bytes": 0
            },
            "reset_local": reset_local
        }))
        .unwrap();
        assert_eq!(
            view.allowance_reset_local(),
            expected,
            "state={state} allowance={allowance} reset_local={reset_local:?}"
        );
    }
}

#[test]
fn waits_for_official_authorization_covers_live_and_repeated_waits() {
    let mut live_official = accepted("live", ShareState::AwaitingAuthorization);
    live_official.authorization = Some(ShareAuthorization::Official);

    let mut repeated_official = accepted("repeated", ShareState::AwaitingAuthorization);
    repeated_official.task_id.clear();
    repeated_official.authorization = Some(ShareAuthorization::Official);

    let mut self_hosted = accepted("self-hosted", ShareState::AwaitingAuthorization);
    self_hosted.task_id.clear();
    self_hosted.authorization = Some(ShareAuthorization::SelfHosted {
        relay_url: "https://relay.example".into(),
    });

    let mut finished_official = accepted("finished", ShareState::Finished);
    finished_official.authorization = Some(ShareAuthorization::Official);

    let mut model = Model::default();
    model.reconcile(snapshot(
        1,
        vec![
            live_official,
            repeated_official,
            self_hosted,
            finished_official,
        ],
    ));

    assert!(model.waits_for_official_authorization("live"));
    assert!(model.waits_for_official_authorization("repeated"));
    assert!(!model.waits_for_official_authorization("self-hosted"));
    assert!(!model.waits_for_official_authorization("finished"));
}
