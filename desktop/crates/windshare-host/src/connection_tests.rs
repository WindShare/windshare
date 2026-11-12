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
