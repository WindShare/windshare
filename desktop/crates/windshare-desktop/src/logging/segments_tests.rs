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

#[test]
fn oversized_record_starts_fresh_after_a_nonempty_segment() {
    const SEGMENT_BYTES: usize = 32;
    const OVERSIZED_RECORD_BYTES: usize = 80;
    let directory = tempfile::tempdir().unwrap();
    let mut writer = Segments::open(directory.path().to_owned()).unwrap();
    writer.limit = SEGMENT_BYTES;
    let first = b"prefix123\n";
    let oversized = [b'x'; OVERSIZED_RECORD_BYTES];
    writer.write_all(first).unwrap();
    writer.write_all(&oversized).unwrap();
    writer.flush().unwrap();
    let mut paths = fs::read_dir(directory.path())
        .unwrap()
        .map(|entry| entry.unwrap().path())
        .collect::<Vec<_>>();
    paths.sort();
    let contents = paths
        .into_iter()
        .map(|path| fs::read(path).unwrap())
        .collect::<Vec<_>>();
    assert_eq!(contents[0], first);
    assert!(
        contents
            .iter()
            .all(|segment| segment.len() <= SEGMENT_BYTES)
    );
    assert_eq!(contents.concat(), [first.as_slice(), &oversized].concat());
}

#[test]
fn records_that_fit_a_fresh_segment_are_not_torn_across_segments() {
    let directory = tempfile::tempdir().unwrap();
    let mut writer = Segments::open(directory.path().to_owned()).unwrap();
    writer.limit = 32;
    // Each call mirrors one formatted tracing line from the non-blocking worker.
    let first: &[u8] = b"first record spans 24 b\n";
    let second: &[u8] = b"second record is 22 b\n";
    writer.write_all(first).unwrap();
    writer.write_all(second).unwrap();
    writer.flush().unwrap();
    let mut contents = fs::read_dir(directory.path())
        .unwrap()
        .map(|entry| fs::read_to_string(entry.unwrap().path()).unwrap())
        .collect::<Vec<_>>();
    contents.sort();
    assert_eq!(
        contents,
        [first, second].map(|line| String::from_utf8(line.to_vec()).unwrap())
    );
}
