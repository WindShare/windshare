use super::*;
use windshare_activation::{EntrySource, ShareIntent};

fn controller() -> (Controller, mpsc::Receiver<Command>) {
    let (commands, receiver) = mpsc::sync_channel(COMMAND_QUEUE_CAPACITY);
    let (updates, receiver_updates) = async_channel::bounded(1);
    (
        Controller {
            shared: Arc::new(Shared {
                model: Mutex::new(Arc::new(Model::default())),
                closing: AtomicBool::new(false),
                account_view_visible: AtomicBool::new(false),
                updates,
                wake: polling::Poller::new().unwrap(),
            }),
            commands,
            sequence: Arc::new(AtomicU64::new(0)),
            worker: Arc::new(Mutex::new(None)),
            updates: receiver_updates,
        },
        receiver,
    )
}

fn intent(name: &str) -> ShareIntent {
    ShareIntent::new(
        EntrySource::Explorer,
        vec![std::env::current_dir().unwrap().join(name)],
    )
}

#[test]
fn selections_are_owned_before_the_host_can_reply() {
    let (controller, receiver) = controller();
    let first = controller.create(intent("file.txt")).unwrap();
    let second = controller.create(intent("folder")).unwrap();
    assert_ne!(first, second);
    let model = controller.model();
    assert_eq!(model.operations.len(), 2);
    assert!(
        model
            .operations
            .iter()
            .all(|op| matches!(op.state, OperationState::Submitting))
    );
    for id in [first, second] {
        let Command::Create { operation_id, .. } = receiver.try_recv().unwrap() else {
            panic!("create command");
        };
        assert_eq!(operation_id, id);
    }
}

#[test]
fn retries_find_the_owned_operation_and_conflicting_content_is_rejected() {
    let (controller, receiver) = controller();
    let first = intent("file.txt");
    let id = controller.create(first.clone()).unwrap();
    assert_eq!(id, controller.create(first.clone()).unwrap());
    let mut conflict = first;
    conflict.roots = intent("different.txt").roots;
    assert!(
        controller
            .create(conflict)
            .unwrap_err()
            .contains("different selection")
    );
    assert_eq!(controller.model().operations.len(), 1);
    assert!(receiver.try_recv().is_ok());
    assert!(receiver.try_recv().is_err());
}

#[test]
fn oversized_selection_is_retained_as_failure_without_queueing() {
    let (controller, receiver) = controller();
    let paths = (0..2048)
        .map(|index| {
            std::env::current_dir()
                .unwrap()
                .join(format!("{index}/{}", "long-path".repeat(20)))
        })
        .collect();
    let id = controller
        .create(ShareIntent::new(EntrySource::Drop, paths))
        .unwrap();
    let model = controller.model();
    assert_eq!(model.operations[0].id, id);
    assert!(
        model.operations[0]
            .error()
            .unwrap()
            .contains("containing folder")
    );
    assert_eq!(model.operations[0].paths.len(), 2048);
    assert!(receiver.try_recv().is_err());
}

#[cfg(windows)]
#[test]
fn unsupported_native_paths_remain_visible_without_unicode_replacement() {
    use std::{ffi::OsString, os::windows::ffi::OsStringExt};
    let (controller, receiver) = controller();
    let path = std::env::current_dir()
        .unwrap()
        .join(OsString::from_wide(&[0xd800]));
    controller
        .create(ShareIntent::new(EntrySource::Explorer, vec![path.clone()]))
        .unwrap();
    let model = controller.model();
    let operation = &model.operations[0];
    assert_eq!(operation.paths, vec![path]);
    assert!(operation.error().unwrap().contains("Unicode"));
    assert!(receiver.try_recv().is_err());
}

#[test]
fn committed_shutdown_rejects_new_work_and_retries() {
    let (controller, receiver) = controller();
    let first = intent("file");
    controller.create(first.clone()).unwrap();
    receiver.try_recv().unwrap();
    controller.begin_shutdown();
    assert!(controller.create(first).is_err());
    assert!(controller.create(intent("later")).is_err());
    assert!(receiver.try_recv().is_err());
}

#[test]
fn retry_after_a_full_local_queue_creates_from_the_retained_selection() {
    let (controller, receiver) = controller();
    for index in 0..COMMAND_QUEUE_CAPACITY {
        controller
            .create(intent(&format!("queued-{index}")))
            .unwrap();
    }
    let selection = intent("retry-me");
    let roots = selection.roots.clone();
    let failed = controller.create(selection).unwrap();
    assert!(
        controller
            .model()
            .operations
            .last()
            .unwrap()
            .error()
            .is_some()
    );
    while receiver.try_recv().is_ok() {}
    let repeated = controller.repeat(&failed).unwrap();
    assert_ne!(failed, repeated);
    let Command::Create {
        operation_id,
        paths,
    } = receiver.try_recv().unwrap()
    else {
        panic!("A local rejection has no host operation to repeat");
    };
    assert_eq!(operation_id, repeated);
    assert_eq!(
        paths,
        roots
            .iter()
            .map(|path| path.to_str().unwrap())
            .collect::<Vec<_>>()
    );
}

#[test]
fn model_notifications_coalesce_and_unchanged_state_does_not_wake() {
    let (controller, _receiver) = controller();
    let initial = controller.model();
    controller
        .shared
        .update(|draft| draft.set_connection(Connection::Connected));
    controller
        .shared
        .update(|draft| draft.set_connection(Connection::Reconnecting("lost".into())));
    assert_eq!(controller.updates.len(), 1);
    controller.updates.try_recv().unwrap();
    let latest = controller.model();
    assert!(!Arc::ptr_eq(&initial, &latest));
    controller
        .shared
        .update(|draft| draft.set_connection(Connection::Reconnecting("lost".into())));
    assert!(controller.updates.try_recv().is_err());
    assert!(Arc::ptr_eq(&latest, &controller.model()));
    controller.refresh_preferences();
    assert!(controller.updates.try_recv().is_err());
    assert!(Arc::ptr_eq(&latest, &controller.model()));
}

#[test]
fn submission_publishes_a_distinct_snapshot_when_previous_is_retained() {
    let (controller, _receiver) = controller();
    let before = controller.model();
    controller.create(intent("file.txt")).unwrap();
    let after = controller.model();
    assert!(!Arc::ptr_eq(&before, &after));
    assert_eq!(before.operations.len(), 0);
    assert_eq!(after.operations.len(), 1);
}

#[test]
fn account_and_setup_submissions_record_pending_and_local_failure_atomically() {
    let (controller, _receiver) = controller();
    let account_id = controller.account(AccountCommand::Refresh).unwrap();
    assert_eq!(
        controller.model().account_request,
        AccountRequest::Pending {
            id: account_id,
            action: AccountAction::Refresh,
        }
    );
    assert!(controller.model().account_busy());
    assert!(controller.model().has_unfinished_work());

    let setup_id = controller.edit_preference("share:1", PreferenceEdit::Mode(RelayMode::Selected));
    assert_eq!(
        controller.model().setup_request,
        SetupRequest::Pending {
            id: setup_id,
            operation_id: "share:1".into(),
            kind: SetupRequestKind::Configure,
        }
    );
    assert!(controller.model().setup_pending());

    controller.begin_shutdown();
    assert!(controller.account(AccountCommand::Refresh).is_none());
    assert!(matches!(
        controller.model().account_request,
        AccountRequest::Failed { .. }
    ));
    assert!(controller.model().command_error.is_none());
    let rejected_id = controller.edit_preference("", PreferenceEdit::Mode(RelayMode::Automatic));
    assert!(matches!(
        &controller.model().setup_request,
        SetupRequest::Rejected { id, .. } if id == &rejected_id
    ));
    assert!(matches!(
        controller.model().setup_settlement(&rejected_id),
        Some(SetupSettlement::Failed(_))
    ));
    assert!(controller.model().command_error.is_none());
}

#[test]
fn cancel_setup_ignores_terminal_and_superseded_setups() {
    let (controller, receiver) = controller();
    let first = controller.edit_preference("", PreferenceEdit::Mode(RelayMode::Selected));
    let _ = receiver.try_recv().unwrap();

    // Terminal setup (Finished): late cancel must not re-enter Pending or queue a command.
    let finished: Snapshot = serde_json::from_value(serde_json::json!({
        "session_id": "session",
        "revision": 1,
        "shares": [],
        "setup": { "state": "finished", "id": first }
    }))
    .unwrap();
    controller
        .shared
        .update(|draft| draft.write().reconcile(finished));
    assert_eq!(controller.model().setup_request, SetupRequest::Idle);
    assert_eq!(
        controller.model().setup_settlement(&first),
        Some(SetupSettlement::Finished)
    );
    assert!(!controller.cancel_setup(&first));
    assert!(receiver.try_recv().is_err());
    assert_eq!(controller.model().setup_request, SetupRequest::Idle);
    assert!(!controller.model().setup_pending());
    assert!(!controller.model().has_unfinished_work());

    // Starting a new setup: cancelling the old setup must not clobber the active one.
    let second = controller.edit_preference("share:2", PreferenceEdit::Mode(RelayMode::Automatic));
    let _ = receiver.try_recv().unwrap();
    assert!(!controller.cancel_setup(&first));
    assert!(receiver.try_recv().is_err());
    assert_eq!(
        controller.model().setup_request,
        SetupRequest::Pending {
            id: second.clone(),
            operation_id: "share:2".into(),
            kind: SetupRequestKind::Configure,
        }
    );

    // Active setup can be cancelled once; duplicate cancel while pending is ignored.
    assert!(controller.cancel_setup(&second));
    assert!(matches!(
        receiver.try_recv().unwrap(),
        Command::CancelSetup { setup_id } if setup_id == second
    ));
    assert_eq!(
        controller.model().setup_request,
        SetupRequest::Pending {
            id: second.clone(),
            operation_id: "share:2".into(),
            kind: SetupRequestKind::Cancel {
                acknowledged: false,
            },
        }
    );
    assert!(!controller.cancel_setup(&second));
    assert!(receiver.try_recv().is_err());

    // Terminal setup (Cancelled): late cancel is also ignored.
    let cancelled: Snapshot = serde_json::from_value(serde_json::json!({
        "session_id": "session",
        "revision": 2,
        "shares": [],
        "setup": { "state": "cancelled", "id": second }
    }))
    .unwrap();
    controller
        .shared
        .update(|draft| draft.write().reconcile(cancelled));
    assert!(!controller.cancel_setup(&second));
    assert!(receiver.try_recv().is_err());
    assert_eq!(controller.model().setup_request, SetupRequest::Idle);
    assert_eq!(
        controller.model().setup_settlement(&second),
        Some(SetupSettlement::Cancelled)
    );
    assert!(!controller.model().has_unfinished_work());
}

#[test]
fn cancel_setup_rejects_superseded_failed_setup_while_retry_is_pending() {
    let (controller, receiver) = controller();
    let first = controller.setup_relay(
        "share:1",
        RelayOperation::AddProfile,
        "https://relay.example".into(),
        String::new(),
    );
    let _ = receiver.try_recv().unwrap();

    // First setup fails with Recovery::Continue: still unfinished and cancellable.
    let failed: Snapshot = serde_json::from_value(serde_json::json!({
        "session_id": "session",
        "revision": 1,
        "shares": [],
        "setup": {
            "state": "failed",
            "id": first,
            "operation_id": "share:1",
            "relay_url": "https://relay.example",
            "relay_operation": "add",
            "recovery": "continue",
            "error": { "code": "setup_failed", "message": "temporary network error" }
        }
    }))
    .unwrap();
    controller
        .shared
        .update(|draft| draft.write().reconcile(failed));
    assert_eq!(controller.model().setup_request, SetupRequest::Idle);
    assert!(controller.model().can_cancel_setup(&first));
    assert!(controller.model().can_cancel_current_setup());

    // User retries (`Configure("setup:2")`): before the host replies, cancelling the
    // superseded `first` setup must be rejected without clobbering `second`.
    let second = controller.setup_relay(
        "share:1",
        RelayOperation::AddProfile,
        "https://relay.example".into(),
        String::new(),
    );
    assert!(matches!(
        receiver.try_recv().unwrap(),
        Command::Configure { setup, .. } if setup.id == second
    ));
    assert!(!controller.model().can_cancel_setup(&first));
    assert!(!controller.model().can_cancel_current_setup());
    assert!(controller.model().can_cancel_setup(&second));
    assert!(!controller.cancel_setup(&first));
    assert!(receiver.try_recv().is_err());
    assert_eq!(
        controller.model().setup_request,
        SetupRequest::Pending {
            id: second,
            operation_id: "share:1".into(),
            kind: SetupRequestKind::Configure,
        }
    );
}

#[test]
fn running_setup_clears_pending_configure_and_remains_cancellable() {
    let (controller, receiver) = controller();
    let active = controller.setup_relay(
        "share:1",
        RelayOperation::AddProfile,
        "https://relay.example".into(),
        String::new(),
    );
    let _ = receiver.try_recv().unwrap();

    // Once the host acknowledges `active` as Running, local Pending(Configure) clears
    // and the active running setup remains cancellable.
    let running: Snapshot = serde_json::from_value(serde_json::json!({
        "session_id": "session",
        "revision": 1,
        "shares": [],
        "setup": {
            "state": "running",
            "id": active,
            "operation_id": "share:1",
            "relay_url": "https://relay.example",
            "relay_operation": "add"
        }
    }))
    .unwrap();
    controller
        .shared
        .update(|draft| draft.write().reconcile(running));
    assert_eq!(controller.model().setup_request, SetupRequest::Idle);
    assert!(!controller.model().setup_pending());
    assert!(controller.model().has_unfinished_work());
    assert_eq!(
        controller.model().in_flight_setup_id("share:1"),
        Some(active.as_str())
    );
    assert!(controller.model().can_cancel_setup(&active));
    assert!(controller.model().can_cancel_current_setup());

    // Cancelling the active running setup succeeds and immediately blocks duplicates.
    assert!(controller.cancel_setup(&active));
    assert!(matches!(
        receiver.try_recv().unwrap(),
        Command::CancelSetup { setup_id } if setup_id == active
    ));
    assert!(!controller.model().can_cancel_current_setup());
    assert!(!controller.cancel_setup(&active));
    assert!(receiver.try_recv().is_err());
}
