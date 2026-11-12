use super::*;

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

#[test]
fn selections_are_owned_before_the_host_can_reply() {
    let (controller, receiver) = controller();
    let first = controller.create(vec![PathBuf::from("file.txt")]).unwrap();
    let second = controller.create(vec![PathBuf::from("folder")]).unwrap();
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
fn oversized_selection_is_rejected_before_queueing() {
    let (controller, receiver) = controller();
    let paths = (0..2048)
        .map(|index| PathBuf::from(format!("{index}/{}", "long-path".repeat(20))))
        .collect();
    assert!(
        controller
            .create(paths)
            .unwrap_err()
            .contains("containing folder")
    );
    assert_eq!(controller.model().operations, Vec::<Operation>::new());
    assert!(receiver.try_recv().is_err());
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
