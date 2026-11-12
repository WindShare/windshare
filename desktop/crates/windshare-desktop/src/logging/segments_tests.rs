use super::*;

#[test]
fn oversized_writes_and_session_restarts_preserve_disk_bounds() {
    let directory = tempfile::tempdir().unwrap();
    for _ in 0..2 {
        let mut writer = Segments::open(directory.path().to_owned()).unwrap();
        writer.limit = 32;
        writer
            .write_all(&[b'x'; 32 * (RETAINED_FILES + 2)])
            .unwrap();
        writer.flush().unwrap();
    }
    let files = fs::read_dir(directory.path())
        .unwrap()
        .map(Result::unwrap)
        .collect::<Vec<_>>();
    assert_eq!(files.len(), RETAINED_FILES);
    assert!(
        files
            .iter()
            .all(|file| file.metadata().unwrap().len() <= 32)
    );
}

#[test]
fn unavailable_log_directory_is_a_recoverable_error() {
    let parent = tempfile::tempdir().unwrap();
    let blocked = parent.path().join("file");
    fs::write(&blocked, b"occupied").unwrap();
    assert!(Segments::open(blocked.join("logs")).is_err());
}
