use super::*;
use windshare_activation::{EntrySource, ShareIntent};

fn controller() -> (Controller, mpsc::Receiver<Command>) {
    let (commands, receiver) = mpsc::sync_channel(COMMAND_QUEUE_CAPACITY);
    let (updates, receiver_updates) = async_channel::bounded(1);
    (
        Controller {
            shared: Arc::new(Shared {
                model: Mutex::new(Model::default()),
                closing: AtomicBool::new(false),
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
    let operation = controller.model().operations.remove(0);
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
    let (controller, _) = controller();
    controller
        .shared
        .update(|model| model.connection = Connection::Connected);
    controller
        .shared
        .update(|model| model.connection = Connection::Reconnecting("lost".into()));
    assert_eq!(controller.updates.len(), 1);
    controller.updates.try_recv().unwrap();
    controller
        .shared
        .update(|model| model.connection = Connection::Reconnecting("lost".into()));
    assert!(controller.updates.try_recv().is_err());
}
