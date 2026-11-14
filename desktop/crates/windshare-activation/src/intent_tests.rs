use super::*;
use std::{ffi::OsString, path::PathBuf};

fn root(name: &str) -> PathBuf {
    std::env::current_dir().unwrap().join(name)
}

#[test]
fn deliberate_actions_have_distinct_ids_and_retries_preserve_native_paths() {
    let selection = ShareIntent::new(EntrySource::Explorer, vec![root("资料"), root("folder")]);
    let retry: ShareIntent = protocol::decode(&protocol::encode(&selection).unwrap()).unwrap();
    assert_eq!(selection, retry);
    assert_ne!(
        selection.id,
        ShareIntent::new(EntrySource::Explorer, selection.roots).id
    );
}

#[cfg(windows)]
#[test]
fn ipc_preserves_unpaired_utf16_for_explicit_host_rejection() {
    use std::os::windows::ffi::OsStringExt;
    let path = root("native").join(OsString::from_wide(&[0xd800]));
    let intent = ShareIntent::new(EntrySource::Explorer, vec![path]);
    assert_eq!(
        protocol::decode::<ShareIntent>(&protocol::encode(&intent).unwrap()).unwrap(),
        intent
    );
}

#[test]
fn command_line_resolves_in_the_invoking_directory_without_scanning() {
    let cwd = root("invoker");
    let args = ["--compact", "--live-share", "资料.txt", "missing/folder"].map(OsString::from);
    let launch::Launch::Application {
        activation: Activation::ShareSelection(intent),
        compact,
    } = launch::parse(args, &cwd).unwrap()
    else {
        panic!("share launch");
    };
    assert!(compact);
    assert_eq!(
        intent.roots,
        vec![cwd.join("资料.txt"), cwd.join("missing/folder")]
    );
}

#[test]
fn invalid_and_oversized_selections_are_never_truncated() {
    assert!(
        ShareIntent::new(EntrySource::Drop, vec![])
            .validate()
            .is_err()
    );
    assert!(
        ShareIntent::new(EntrySource::Drop, vec![PathBuf::from("relative")])
            .validate()
            .is_err()
    );
    let mut intent = ShareIntent::new(
        EntrySource::Explorer,
        vec![root("file"); MAX_SELECTED_ROOTS],
    );
    assert!(intent.validate().is_ok());
    intent.roots.push(root("extra"));
    assert!(intent.validate().is_err());
    assert!(protocol::decode::<Activation>(&vec![0; protocol::MAX_MESSAGE_BYTES + 1]).is_err());
}

#[test]
fn empty_startup_selection_and_conflicting_modes_are_errors() {
    let cwd = root("invoker");
    for args in [
        vec!["--live-share"],
        vec!["--preview", "--live-share", "file"],
        vec!["--host-check", "file"],
    ] {
        assert!(launch::parse(args.into_iter().map(OsString::from), &cwd).is_err());
    }
}
