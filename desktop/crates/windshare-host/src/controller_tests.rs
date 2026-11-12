use super::*;

fn controller() -> (Controller, mpsc::Receiver<Command>) {
    let (commands, receiver) = mpsc::sync_channel(COMMAND_QUEUE_CAPACITY);
    (
        Controller {
            shared: Arc::new(Shared {
                model: Mutex::new(Model::default()),
                closing: AtomicBool::new(false),
            }),
            commands,
            sequence: Arc::new(AtomicU64::new(0)),
            worker: Arc::new(Mutex::new(None)),
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
