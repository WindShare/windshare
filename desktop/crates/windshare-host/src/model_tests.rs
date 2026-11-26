use super::*;

#[test]
fn share_wire_contract_enforces_state_specific_payloads() {
    let source = serde_json::json!({
        "operation_id": "selection", "state": "awaiting_network",
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
            error: None,
        }
    );
    for invalid in [
        serde_json::json!({ "operation_id": "selection", "state": "awaiting_network" }),
        serde_json::json!({ "operation_id": "selection", "state": "awaiting_authorization" }),
        serde_json::json!({ "operation_id": "selection", "state": "sharing", "task_id": "engine:1" }),
        serde_json::json!({ "operation_id": "selection", "state": "finished" }),
    ] {
        assert!(
            serde_json::from_value::<Share>(invalid.clone()).is_err(),
            "accepted illegal wire share: {invalid}"
        );
    }
    let setup: Setup = serde_json::from_value(serde_json::json!({
        "state": "finished", "id": "setup:1"
    }))
    .unwrap();
    assert!(
        matches!(setup, Setup::Finished { context } if context.operation_id.is_empty() && !context.relay_removed)
    );
    let removed_setup: Setup = serde_json::from_value(serde_json::json!({
        "state": "finished", "id": "setup:2", "operation_id": "share:1", "relay_removed": true
    }))
    .unwrap();
    assert!(
        matches!(removed_setup, Setup::Finished { context } if context.operation_id == "share:1" && context.relay_removed)
    );

    let with_backup: Share = serde_json::from_value(serde_json::json!({
        "operation_id": "share", "state": "sharing", "task_id": "engine:1",
        "url": "https://ws.example/id#key", "backup_url": "https://backup.example/id#key",
        "relay": { "available": 1, "total": 2, "ever_ready": true, "terminal": 0 }
    }))
    .unwrap();
    assert_eq!(
        with_backup.state,
        ShareState::Sharing {
            task_id: "engine:1".into(),
            url: "https://ws.example/id#key".into(),
            backup_url: Some("https://backup.example/id#key".into()),
            relay: Some(RelayAvailability {
                available: 1,
                total: 2,
                ever_ready: true,
                terminal: 0,
            }),
        }
    );

    let frontend_required: Share = serde_json::from_value(serde_json::json!({
        "operation_id": "missing-front",
        "state": "awaiting_configuration",
        "blocking": { "reason": "frontend_required", "recovery": "configure" },
        "error": { "code": "frontend_required", "message": "configure a receiver website before sharing" }
    }))
    .unwrap();
    assert_eq!(
        frontend_required.state,
        ShareState::AwaitingConfiguration {
            blocking: CreationBlock {
                reason: BlockingReason::FrontendRequired,
                recovery: RecoveryAction::Configure,
                retry_at_ms: None,
            },
            error: Some(Failure::new(
                "frontend_required",
                "configure a receiver website before sharing",
            )),
        }
    );
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
        created_at_ms: 0,
        selection: None,
        omissions: None,
        state,
    }
}

fn sharing(id: &str) -> ShareState {
    ShareState::Sharing {
        task_id: format!("engine:{id}"),
        url: "https://example.test/share#key".into(),
        backup_url: None,
        relay: None,
    }
}

fn stopping(id: &str) -> ShareState {
    ShareState::Stopping {
        task_id: Some(format!("engine:{id}")),
    }
}

fn finished(outcome: Outcome, authorization: Option<ShareAuthorization>) -> ShareState {
    ShareState::Finished {
        result: Completion {
            outcome,
            stop_reason: StopReason::None,
            error: String::new(),
            cleanup_error: String::new(),
        },
        error: None,
        authorization,
    }
}

#[test]
fn snapshots_cannot_move_backwards_or_cross_host_sessions() {
    let mut model = Model::default();
    model.reconcile(snapshot(5, vec![accepted("one", stopping("one"))]));
    model.reconcile(snapshot(3, vec![accepted("one", sharing("one"))]));
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
    assert_eq!(model.operations[0].share().unwrap().state, stopping("one"));
    model.reconcile(snapshot(
        6,
        vec![accepted("one", finished(Outcome::Stopped, None))],
    ));
    assert_eq!(
        model.operations[0].share().unwrap().state,
        finished(Outcome::Stopped, None)
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
    model.reconcile(snapshot(2, vec![accepted("one", sharing("one"))]));
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
            accepted("one", sharing("one")),
            accepted("two", sharing("two")),
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
    let official = accepted(
        "official",
        ShareState::AwaitingAuthorization {
            authorization: ShareAuthorization::Official,
        },
    );
    let self_hosted = accepted(
        "self-hosted",
        ShareState::AwaitingAuthorization {
            authorization: ShareAuthorization::SelfHosted {
                relay_url: "https://relay.example".into(),
            },
        },
    );
    let finished_official = accepted(
        "finished",
        finished(Outcome::Failed, Some(ShareAuthorization::Official)),
    );

    let mut model = Model::default();
    model.reconcile(snapshot(1, vec![official, self_hosted, finished_official]));

    assert!(model.waits_for_official_authorization("official"));
    assert!(!model.waits_for_official_authorization("self-hosted"));
    assert!(!model.waits_for_official_authorization("finished"));
}

#[test]
fn setup_cancel_request_settles_if_setup_finishes_first() {
    let mut model = Model {
        setup_request: crate::SetupRequest::Pending {
            id: "setup:1".into(),
            operation_id: String::new(),
            kind: crate::SetupRequestKind::Cancel {
                acknowledged: false,
            },
        },
        ..Model::default()
    };
    let finished_setup: Setup = serde_json::from_value(serde_json::json!({
        "state": "finished",
        "id": "setup:1",
        "relay_saved": true
    }))
    .unwrap();
    let mut next = snapshot(1, vec![]);
    next.setup = finished_setup;
    model.reconcile(next);
    assert_eq!(model.setup_request, crate::SetupRequest::Idle);
    assert_eq!(
        model.setup_settlement("setup:1"),
        Some(crate::SetupSettlement::Finished)
    );
}

#[test]
fn setup_settlement_reconciles_host_state_without_local_request_mirror() {
    let mut model = Model::default();

    // Adopted busy setup with SetupRequest::Idle stays unsettled while busy,
    // then settles directly from authoritative host snapshots.
    let mut running = snapshot(1, vec![]);
    running.setup = serde_json::from_value(serde_json::json!({
        "state": "running",
        "id": "setup:1",
        "operation_id": "share:a",
        "relay_url": "https://relay.example"
    }))
    .unwrap();
    model.reconcile(running);
    assert_eq!(model.in_flight_setup_id("share:a"), Some("setup:1"));
    assert_eq!(model.setup_settlement("setup:1"), None);

    let mut finished = snapshot(2, vec![]);
    finished.setup = serde_json::from_value(serde_json::json!({
        "state": "finished",
        "id": "setup:1",
        "operation_id": "share:a",
        "relay_saved": true
    }))
    .unwrap();
    model.reconcile(finished);
    assert_eq!(
        model.setup_settlement("setup:1"),
        Some(crate::SetupSettlement::Finished)
    );
    assert_eq!(model.setup_settlement("setup:other"), None);
    assert_eq!(model.setup_settlement(""), None);

    // Unfinished Failed(Cancel) clears pending request without settling early;
    // when share cancellation drives the same setup to Cancelled, it settles cleanly.
    model.setup_request = crate::SetupRequest::Pending {
        id: "setup:2".into(),
        operation_id: "share:a".into(),
        kind: crate::SetupRequestKind::Configure,
    };
    let mut cancel_required = snapshot(3, vec![]);
    cancel_required.setup = serde_json::from_value(serde_json::json!({
        "state": "failed",
        "id": "setup:2",
        "operation_id": "share:a",
        "recovery": "cancel",
        "error": { "code": "setup_failed", "message": "settings changed" }
    }))
    .unwrap();
    model.reconcile(cancel_required);
    assert_eq!(model.setup_request, crate::SetupRequest::Idle);
    assert_eq!(model.setup_settlement("setup:2"), None);

    let mut cancelling = snapshot(4, vec![]);
    cancelling.setup = serde_json::from_value(serde_json::json!({
        "state": "cancelling",
        "id": "setup:2",
        "operation_id": "share:a"
    }))
    .unwrap();
    model.reconcile(cancelling);
    assert_eq!(model.in_flight_setup_id("share:a"), Some("setup:2"));
    assert_eq!(model.setup_settlement("setup:2"), None);

    let mut cancelled = snapshot(5, vec![]);
    cancelled.setup = serde_json::from_value(serde_json::json!({
        "state": "cancelled",
        "id": "setup:2",
        "operation_id": "share:a"
    }))
    .unwrap();
    model.reconcile(cancelled);
    assert_eq!(
        model.setup_settlement("setup:2"),
        Some(crate::SetupSettlement::Cancelled)
    );

    // Terminal host failure (recovery: restart) settles as Failed from host state.
    let mut failed = snapshot(6, vec![]);
    failed.setup = serde_json::from_value(serde_json::json!({
        "state": "failed",
        "id": "setup:3",
        "recovery": "restart",
        "error": { "code": "setup_failed", "message": "relay unreachable" }
    }))
    .unwrap();
    model.reconcile(failed);
    assert_eq!(
        model.setup_settlement("setup:3"),
        Some(crate::SetupSettlement::Failed(&Failure::new(
            "setup_failed",
            "relay unreachable"
        )))
    );

    // A rejected CancelSetup while host is still Running must not settle early
    // or override a subsequent authoritative Finished snapshot.
    let mut still_running = snapshot(7, vec![]);
    still_running.setup = serde_json::from_value(serde_json::json!({
        "state": "running",
        "id": "setup:4",
        "relay_url": "https://relay.example"
    }))
    .unwrap();
    model.reconcile(still_running);
    model.setup_request = crate::SetupRequest::Rejected {
        id: "setup:4".into(),
        error: Failure::new("request_rejected", "cancel command rejected"),
    };
    assert_eq!(model.setup_settlement("setup:4"), None);

    let mut completed_after_cancel_reject = snapshot(8, vec![]);
    completed_after_cancel_reject.setup = serde_json::from_value(serde_json::json!({
        "state": "finished",
        "id": "setup:4",
        "relay_saved": true
    }))
    .unwrap();
    model.reconcile(completed_after_cancel_reject);
    assert_eq!(model.setup_request, crate::SetupRequest::Idle);
    assert_eq!(
        model.setup_settlement("setup:4"),
        Some(crate::SetupSettlement::Finished)
    );

    // Local/host rejection for a command that never entered Model::setup settles as Failed.
    model.setup_request = crate::SetupRequest::Rejected {
        id: "setup:5".into(),
        error: Failure::new("request_rejected", "invalid relay URL"),
    };
    assert_eq!(
        model.setup_settlement("setup:5"),
        Some(crate::SetupSettlement::Failed(&Failure::new(
            "request_rejected",
            "invalid relay URL"
        )))
    );
}

#[test]
fn in_flight_setup_id_respects_operation_ownership() {
    let mut model = Model {
        setup_request: crate::SetupRequest::Pending {
            id: "setup:1".into(),
            operation_id: "share:a".into(),
            kind: crate::SetupRequestKind::Configure,
        },
        ..Model::default()
    };
    assert_eq!(model.in_flight_setup_id("share:a"), Some("setup:1"));
    assert_eq!(model.in_flight_setup_id("share:b"), None);
    assert_eq!(model.in_flight_setup_id(""), Some("setup:1"));

    model.setup_request = crate::SetupRequest::Pending {
        id: "setup:2".into(),
        operation_id: String::new(),
        kind: crate::SetupRequestKind::Configure,
    };
    assert_eq!(model.in_flight_setup_id("share:b"), None);
    assert_eq!(model.in_flight_setup_id(""), Some("setup:2"));

    // After reconnect, a busy host setup without a local pending request still enforces ownership.
    model.setup_request = crate::SetupRequest::Idle;
    model.setup = serde_json::from_value(serde_json::json!({
        "state": "running",
        "id": "setup:3",
        "operation_id": "share:a",
        "relay_url": "https://relay.example"
    }))
    .unwrap();
    assert_eq!(model.in_flight_setup_id("share:a"), Some("setup:3"));
    assert_eq!(model.in_flight_setup_id("share:b"), None);
    assert_eq!(model.in_flight_setup_id(""), Some("setup:3"));

    model.setup = serde_json::from_value(serde_json::json!({
        "state": "finished",
        "id": "setup:3",
        "operation_id": "share:a",
        "relay_saved": true
    }))
    .unwrap();
    assert_eq!(model.in_flight_setup_id("share:a"), None);
    assert_eq!(model.in_flight_setup_id(""), None);
}

#[test]
fn structured_share_failure_and_omissions_wire_contract_deserializes() {
    let failed: Share = serde_json::from_value(serde_json::json!({
        "operation_id": "cloud-op",
        "state": "finished",
        "result": { "outcome": "failed", "stop_reason": "none", "error": "fallback" },
        "error": {
            "code": "startup_failed",
            "message": "fallback",
            "engine_code": "source_cloud_placeholder",
            "source_path": "C:\\Users\\alice\\OneDrive\\deck.pptx"
        }
    }))
    .unwrap();
    let failure = failed.failure().unwrap();
    assert_eq!(failure.code, "startup_failed");
    assert_eq!(
        failure.engine_code.as_deref(),
        Some("source_cloud_placeholder")
    );
    assert_eq!(
        failure.source_path.as_deref(),
        Some("C:\\Users\\alice\\OneDrive\\deck.pptx")
    );

    let sharing_with_omissions: Share = serde_json::from_value(serde_json::json!({
        "operation_id": "mixed-op",
        "state": "sharing",
        "task_id": "engine:1",
        "url": "https://ws.example/id#key",
        "omissions": {
            "count": 2,
            "samples": [
                { "name": "remote.docx", "reason": "cloud_placeholder" }
            ]
        }
    }))
    .unwrap();
    let omissions = sharing_with_omissions.omissions.unwrap();
    assert_eq!(omissions.count, 2);
    assert_eq!(omissions.samples.len(), 1);
    assert_eq!(omissions.samples[0].name, "remote.docx");
    assert_eq!(omissions.samples[0].reason, "cloud_placeholder");
}

#[test]
fn in_flight_registration_id_tracks_only_pending_register_commands() {
    let mut model = Model {
        account_request: crate::AccountRequest::Pending {
            id: "account:1".into(),
            action: crate::AccountAction::Register,
        },
        ..Model::default()
    };
    assert_eq!(model.in_flight_registration_id(), Some("account:1"));

    for action in [
        crate::AccountAction::Login,
        crate::AccountAction::Password,
        crate::AccountAction::Nickname,
        crate::AccountAction::Refresh,
    ] {
        model.account_request = crate::AccountRequest::Pending {
            id: "account:2".into(),
            action,
        };
        assert_eq!(model.in_flight_registration_id(), None, "{action:?}");
    }

    model.account_request = crate::AccountRequest::Pending {
        id: "account:3".into(),
        action: crate::AccountAction::Register,
    };
    let mut failed_snapshot = snapshot(1, vec![]);
    failed_snapshot.account = Some(
        serde_json::from_value(serde_json::json!({
            "status": {
                "active": true,
                "state": "registration_required",
                "allowance": "stale",
                "remaining_bytes": 0
            },
            "operation": {
                "id": "account:3",
                "state": "failed",
                "error": { "code": "rate_limited", "message": "try again later" }
            }
        }))
        .unwrap(),
    );
    model.reconcile(failed_snapshot);
    assert_eq!(model.in_flight_registration_id(), None);
    assert_eq!(
        model.account_request,
        crate::AccountRequest::Failed {
            id: "account:3".into(),
            error: Failure::new("rate_limited", "try again later"),
        }
    );
}

#[test]
fn can_cancel_setup_enforces_state_and_pending_request_ownership() {
    let mut model = Model::default();
    assert!(!model.can_cancel_setup(""));
    assert!(!model.can_cancel_current_setup());

    // Active Running setup is cancellable both before and after local Pending(Configure) clears.
    let mut running = snapshot(1, vec![]);
    running.setup = serde_json::from_value(serde_json::json!({
        "state": "running",
        "id": "setup:1",
        "operation_id": "share:a",
        "relay_url": "https://relay.example"
    }))
    .unwrap();
    model.setup_request = crate::SetupRequest::Pending {
        id: "setup:1".into(),
        operation_id: "share:a".into(),
        kind: crate::SetupRequestKind::Configure,
    };
    assert!(model.can_cancel_setup("setup:1"));
    model.reconcile(running);
    assert_eq!(model.setup_request, crate::SetupRequest::Idle);
    assert!(model.can_cancel_setup("setup:1"));
    assert!(model.can_cancel_current_setup());
    assert!(!model.can_cancel_setup("setup:other"));

    // An adopted Cancelling setup (e.g. after reconnect with Idle request) is not re-cancellable.
    let mut cancelling = snapshot(2, vec![]);
    cancelling.setup = serde_json::from_value(serde_json::json!({
        "state": "cancelling",
        "id": "setup:1",
        "operation_id": "share:a"
    }))
    .unwrap();
    model.reconcile(cancelling);
    assert!(!model.can_cancel_setup("setup:1"));
    assert!(!model.can_cancel_current_setup());

    // Terminal failures (Restart / Blocked) have no pending enrollment to cancel.
    for (rev, recovery) in [(3, "restart"), (4, "blocked")] {
        let mut terminal_fail = snapshot(rev, vec![]);
        terminal_fail.setup = serde_json::from_value(serde_json::json!({
            "state": "failed",
            "id": format!("setup:{rev}"),
            "recovery": recovery,
            "error": { "code": "setup_failed", "message": "failed" }
        }))
        .unwrap();
        model.reconcile(terminal_fail);
        assert!(
            !model.can_cancel_setup(&format!("setup:{rev}")),
            "{recovery}"
        );
        assert!(!model.can_cancel_current_setup(), "{recovery}");
    }

    // CancelFailed is cancellable when idle, but disabled once a cancel or newer configure is pending.
    let mut cancel_failed = snapshot(5, vec![]);
    cancel_failed.setup = serde_json::from_value(serde_json::json!({
        "state": "cancel_failed",
        "id": "setup:5",
        "error": { "code": "setup_cancel_failed", "message": "offline" }
    }))
    .unwrap();
    model.reconcile(cancel_failed);
    assert!(model.can_cancel_setup("setup:5"));
    assert!(model.can_cancel_current_setup());

    model.setup_request = crate::SetupRequest::Pending {
        id: "setup:5".into(),
        operation_id: String::new(),
        kind: crate::SetupRequestKind::Cancel {
            acknowledged: false,
        },
    };
    assert!(!model.can_cancel_setup("setup:5"));
    assert!(!model.can_cancel_current_setup());
}
