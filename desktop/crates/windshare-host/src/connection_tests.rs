use super::*;
use crate::{Model, Operation, OperationState};
use std::sync::{Mutex, atomic::AtomicBool};

fn pending_create() -> (Shared, BTreeMap<u64, Request>) {
    let mut model = Model::default();
    model.operations.push(Operation {
        id: "selection".into(),
        paths: vec!["file.txt".into()],
        state: OperationState::Submitting,
    });
    let shared = Shared {
        model: Mutex::new(model),
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
fn observations_never_acknowledge_unanswered_commands() {
    let (shared, mut pending) = pending_create();
    receive(
        &shared,
        &mut pending,
        r#"{"version":1,"observation":{"task_id":"engine:1"}}"#,
    )
    .unwrap();
    assert_eq!(pending.len(), 1);
    receive(
        &shared,
        &mut pending,
        r#"{"version":1,"id":"1","snapshot":{"session_id":"session","revision":1,"shares":[]}}"#,
    )
    .unwrap();
    assert!(pending.is_empty());
    assert_eq!(shared.model.lock().unwrap().revision, 1);
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
    let commands = [
        Command::Account {
            operation_id: "account:1".into(),
            account: crate::AccountCommand::Retry,
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
    assert_eq!(model.settings_rejection("account:1").unwrap().message, "2");
    assert_eq!(model.settings_rejection("setup:2").unwrap().message, "3");
    assert!(model.settings_rejection("account:3").is_none());
    assert_eq!(model.command_error.as_deref(), Some("4"));
}
