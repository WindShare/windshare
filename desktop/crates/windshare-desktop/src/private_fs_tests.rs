use super::*;
use std::io::Write;

#[test]
fn create_dir_all_creates_nested_directories_and_rejects_file_ancestor() {
    let base = tempfile::tempdir().unwrap();
    let nested = base.path().join("windshare").join("desktop");
    create_dir_all(&nested).unwrap();
    assert!(nested.is_dir());

    let blocked = base.path().join("occupied");
    fs::write(&blocked, b"not a directory").unwrap();
    assert!(create_dir_all(&blocked.join("child")).is_err());
}

#[test]
fn create_new_file_creates_writable_file_and_rejects_existing_path() {
    let base = tempfile::tempdir().unwrap();
    let path = base.path().join("segment.log");
    let mut file = create_new_file(&path).unwrap();
    file.write_all(b"hello").unwrap();
    file.flush().unwrap();
    drop(file);

    assert_eq!(fs::read(&path).unwrap(), b"hello");
    let error = create_new_file(&path).unwrap_err();
    assert_eq!(error.kind(), io::ErrorKind::AlreadyExists);
}

#[cfg(unix)]
#[test]
fn unix_create_dir_all_restricts_new_directories_without_altering_existing_ones() {
    use std::os::unix::fs::PermissionsExt;

    let base = tempfile::tempdir().unwrap();
    let existing = base.path().join("existing");
    fs::create_dir(&existing).unwrap();
    fs::set_permissions(&existing, fs::Permissions::from_mode(0o755)).unwrap();

    let nested_root = existing.join("windshare");
    let nested_leaf = nested_root.join("desktop");
    create_dir_all(&nested_leaf).unwrap();

    assert_eq!(
        fs::metadata(&existing).unwrap().permissions().mode() & 0o777,
        0o755
    );
    assert_eq!(
        fs::metadata(&nested_root).unwrap().permissions().mode() & 0o777,
        PRIVATE_DIRECTORY_MODE
    );
    assert_eq!(
        fs::metadata(&nested_leaf).unwrap().permissions().mode() & 0o777,
        PRIVATE_DIRECTORY_MODE
    );

    // Re-invoking on an existing target preserves its pre-existing permissions.
    create_dir_all(&existing).unwrap();
    assert_eq!(
        fs::metadata(&existing).unwrap().permissions().mode() & 0o777,
        0o755
    );
}

#[cfg(unix)]
#[test]
fn unix_create_new_file_applies_owner_only_mode() {
    use std::os::unix::fs::PermissionsExt;

    let base = tempfile::tempdir().unwrap();
    let path = base.path().join("private.log");
    drop(create_new_file(&path).unwrap());
    assert_eq!(
        fs::metadata(&path).unwrap().permissions().mode() & 0o777,
        PRIVATE_FILE_MODE
    );
}
