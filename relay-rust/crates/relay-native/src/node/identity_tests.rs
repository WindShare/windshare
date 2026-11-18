use super::*;

#[test]
fn concurrent_first_start_preserves_one_complete_identity() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("node.json");
    let identities = std::thread::scope(|scope| {
        let tasks: [_; 8] = std::array::from_fn(|_| scope.spawn(|| Identity::open(&path).unwrap()));
        tasks
            .into_iter()
            .map(|task| task.join().unwrap())
            .collect::<Vec<_>>()
    });
    let saved = Identity::open(&path).unwrap();
    for identity in identities {
        assert_eq!(identity.id(), saved.id());
        assert_eq!(identity.owner().digest, saved.owner().digest);
    }
    assert_eq!(std::fs::read_dir(directory.path()).unwrap().count(), 1);
}

#[test]
fn damaged_identity_is_not_replaced() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("node.json");
    std::fs::write(&path, b"truncated").unwrap();
    assert!(Identity::open(&path).is_err());
    assert_eq!(std::fs::read(&path).unwrap(), b"truncated");
}
