use super::*;
use crate::{Model, Operation, OperationState};
use std::sync::{Arc, Mutex, atomic::AtomicBool};

fn pending_create() -> (Shared, BTreeMap<u64, Request>) {
    let mut model = Model::default();
    model.operations.push(Operation {
        id: "selection".into(),
        paths: vec!["file.txt".into()],
        state: OperationState::Submitting,
    });
    let shared = Shared {
        model: Mutex::new(Arc::new(model)),
        closing: AtomicBool::new(false),
        account_view_visible: AtomicBool::new(false),
        updates: async_channel::bounded(1).0,
        wake: polling::Poller::new().unwrap(),
    };
    let pending = BTreeMap::from([(
        1,
        Request {
            id: "1".into(),
            command: Command::Create {
                operation_id: "selection".into(),
                paths: vec!["file.txt".into()],
            },
        },
    )]);
    (shared, pending)
}

#[test]
fn empty_control_frames_never_acknowledge_unanswered_commands() {
    let (shared, mut pending) = pending_create();
    receive(&shared, &mut pending, r#"{"version":1}"#).unwrap();
    assert_eq!(pending.len(), 1);
    receive(
        &shared,
        &mut pending,
        r#"{"version":1,"id":"1","snapshot":{"session_id":"session","revision":1,"shares":[]}}"#,
    )
    .unwrap();
    assert!(pending.is_empty());
    let accepted = Arc::clone(&shared.model.lock().unwrap());
    assert_eq!(accepted.revision, 1);
    receive(
        &shared,
        &mut pending,
        r#"{"version":1,"snapshot":{"session_id":"session","revision":1,"shares":[]}}"#,
    )
    .unwrap();
    assert!(Arc::ptr_eq(&accepted, &shared.model.lock().unwrap()));
}

#[test]
fn rejected_creation_settles_its_selection_instead_of_preparing_forever() {
    let (shared, mut pending) = pending_create();
    receive(&shared, &mut pending, r#"{"version":1,"id":"1","error":{"code":"request_rejected","message":"selection rejected"}}"#).unwrap();
    let model = shared.model.lock().unwrap();
    assert!(matches!(
        model.operations[0].state,
        OperationState::Rejected(_)
    ));
    assert_eq!(model.operations[0].error(), Some("selection rejected"));
    assert!(pending.is_empty());
}

#[test]
fn a_rejected_settings_submission_retains_its_own_failure() {
    let (shared, mut pending) = pending_create();
    shared.update(|draft| {
        let model = draft.write();
        model.account_request = crate::AccountRequest::Pending {
            id: "account:1".into(),
        };
        model.setup_request = crate::SetupRequest::Pending {
            id: "setup:2".into(),
            operation_id: String::new(),
            kind: crate::SetupRequestKind::Configure,
        };
    });
    let commands = [
        Command::Account {
            operation_id: "account:99".into(),
            account: crate::AccountCommand::Refresh,
        },
        Command::Account {
            operation_id: "account:1".into(),
            account: crate::AccountCommand::Refresh,
        },
        Command::Configure {
            operation_id: String::new(),
            setup: crate::request::SetupInput {
                id: "setup:2".into(),
                ..Default::default()
            },
        },
        Command::Preferences,
    ];
    for (i, command) in commands.into_iter().enumerate() {
        let sequence = u64::try_from(i).unwrap() + 2;
        let id = sequence.to_string();
        pending.insert(
            sequence,
            Request {
                id: id.clone(),
                command,
            },
        );
        let response = serde_json::json!({
            "version": 1,
            "id": id,
            "error": { "code": "request_rejected", "message": id },
        });
        receive(&shared, &mut pending, &response.to_string()).unwrap();
    }
    let model = shared.model.lock().unwrap();
    assert_eq!(
        model.account_request,
        crate::AccountRequest::Failed {
            id: "account:1".into(),
            error: crate::Failure::new("request_rejected", "3"),
        }
    );
    assert_eq!(
        model.setup_request,
        crate::SetupRequest::Failed {
            id: "setup:2".into(),
            error: crate::Failure::new("request_rejected", "4"),
        }
    );
    assert_eq!(model.command_error.as_deref(), Some("5"));
}

#[test]
fn cancel_setup_ignores_stale_failure_until_acknowledged() {
    let (shared, mut pending) = pending_create();
    shared.update(|draft| {
        draft.write().setup_request = crate::SetupRequest::Pending {
            id: "setup:1".into(),
            operation_id: "share:a".into(),
            kind: crate::SetupRequestKind::Cancel {
                acknowledged: false,
            },
        };
    });
    let stale_snapshot = serde_json::json!({
        "version": 1,
        "snapshot": {
            "session_id": "session",
            "revision": 1,
            "shares": [],
            "setup": {
                "state": "cancel_failed",
                "id": "setup:1",
                "error": { "code": "setup_cancel_failed", "message": "old failure" }
            }
        }
    });
    receive(&shared, &mut pending, &stale_snapshot.to_string()).unwrap();
    assert!(shared.model.lock().unwrap().setup_pending());
    assert_eq!(
        shared.model.lock().unwrap().in_flight_setup_id("share:a"),
        Some("setup:1")
    );
    assert_eq!(
        shared.model.lock().unwrap().in_flight_setup_id("share:b"),
        None
    );

    pending.insert(
        2,
        Request {
            id: "2".into(),
            command: Command::CancelSetup {
                setup_id: "setup:1".into(),
            },
        },
    );
    let cancelling = serde_json::json!({
        "version": 1,
        "id": "2",
        "snapshot": {
            "session_id": "session",
            "revision": 2,
            "shares": [],
            "setup": {
                "state": "cancelling",
                "id": "setup:1",
                "operation_id": "share:a"
            }
        }
    });
    receive(&shared, &mut pending, &cancelling.to_string()).unwrap();
    assert_eq!(
        shared.model.lock().unwrap().setup_request,
        crate::SetupRequest::Pending {
            id: "setup:1".into(),
            operation_id: "share:a".into(),
            kind: crate::SetupRequestKind::Cancel { acknowledged: true },
        }
    );

    let settled = serde_json::json!({
        "version": 1,
        "snapshot": {
            "session_id": "session",
            "revision": 3,
            "shares": [],
            "setup": {
                "state": "cancel_failed",
                "id": "setup:1",
                "operation_id": "share:a",
                "error": { "code": "setup_cancel_failed", "message": "new failure" }
            }
        }
    });
    receive(&shared, &mut pending, &settled.to_string()).unwrap();
    assert_eq!(
        shared.model.lock().unwrap().setup_request,
        crate::SetupRequest::Failed {
            id: "setup:1".into(),
            error: crate::Failure::new("setup_cancel_failed", "new failure"),
        }
    );
}

#[test]
fn cancel_setup_same_revision_reply_settles_against_authoritative_setup() {
    let cases = [
        (
            "finished",
            serde_json::json!({ "state": "finished", "id": "setup:1", "relay_saved": true }),
            crate::SetupRequest::Finished {
                id: "setup:1".into(),
            },
        ),
        (
            "cancelled",
            serde_json::json!({ "state": "cancelled", "id": "setup:1" }),
            crate::SetupRequest::Cancelled {
                id: "setup:1".into(),
            },
        ),
        (
            "superseded",
            serde_json::json!({ "state": "finished", "id": "setup:2" }),
            crate::SetupRequest::Idle,
        ),
    ];

    for (label, setup_json, expected) in cases {
        let shared = Shared {
            model: Mutex::new(Arc::new(Model::default())),
            closing: AtomicBool::new(false),
            account_view_visible: AtomicBool::new(false),
            updates: async_channel::bounded(1).0,
            wake: polling::Poller::new().unwrap(),
        };
        let mut pending = BTreeMap::new();
        let initial = serde_json::json!({
            "version": 1,
            "snapshot": {
                "session_id": "session",
                "revision": 5,
                "shares": [],
                "setup": setup_json,
            }
        });
        receive(&shared, &mut pending, &initial.to_string()).unwrap();
        shared.update(|draft| {
            draft.write().setup_request = crate::SetupRequest::Pending {
                id: "setup:1".into(),
                operation_id: String::new(),
                kind: crate::SetupRequestKind::Cancel {
                    acknowledged: false,
                },
            };
        });
        pending.insert(
            2,
            Request {
                id: "2".into(),
                command: Command::CancelSetup {
                    setup_id: "setup:1".into(),
                },
            },
        );
        let reply = serde_json::json!({
            "version": 1,
            "id": "2",
            "snapshot": {
                "session_id": "session",
                "revision": 5,
                "shares": [],
                "setup": setup_json,
            }
        });
        receive(&shared, &mut pending, &reply.to_string()).unwrap();
        let model = Arc::clone(&shared.model.lock().unwrap());
        assert_eq!(model.setup_request, expected, "{label}");
        assert!(!model.setup_pending(), "{label}");
        assert!(!model.has_unfinished_work(), "{label}");
    }
}
