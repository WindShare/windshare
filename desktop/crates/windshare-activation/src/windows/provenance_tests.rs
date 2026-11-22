use super::*;
use std::{process::Command, time::Duration};
use windows::Win32::{
    Storage::FileSystem::{FILE_ATTRIBUTE_NORMAL, GetShortPathNameW, SetFileAttributesW},
    System::WinRT::RO_INIT_MULTITHREADED,
};

#[test]
fn path_containment_uses_native_case_insensitive_components() {
    let root = Path::new(r"\\?\C:\Users\Example\AppData\Local\Temp");
    assert!(beneath(
        Path::new(r"\\?\c:\users\example\APPDATA\LOCAL\TEMP\shot.png"),
        root
    ));
    assert!(!beneath(
        Path::new(r"\\?\C:\Users\Example\AppData\Local\Temp-backup\shot.png"),
        root
    ));
    assert!(!beneath(
        Path::new(r"\\?\C:\Users\Example\AppData\Local\Temp\..\keep.txt"),
        root
    ));
    assert!(!beneath(Path::new(r"Temp\shot.png"), root));
    assert_eq!(
        verbatim_dos_path(Path::new(r"C:\Windows\Temp")),
        Some(PathBuf::from(r"\\?\C:\Windows\Temp"))
    );
    assert_eq!(
        verbatim_dos_path(Path::new(r"\\?\C:\Windows\Temp")),
        Some(PathBuf::from(r"\\?\C:\Windows\Temp"))
    );
    assert_eq!(verbatim_dos_path(Path::new(r"Temp\shot.png")), None);
}

struct Fixture(PathBuf);
impl Drop for Fixture {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

fn open_file(path: &Path, deadline: Instant) -> IStorageItem {
    let native = HSTRING::from(path.as_os_str());
    wait(
        &StorageFile::GetFileFromPathAsync(&native).unwrap(),
        deadline,
    )
    .unwrap()
    .cast()
    .unwrap()
}

fn open_folder(path: &Path, deadline: Instant) -> IStorageItem {
    let native = HSTRING::from(path.as_os_str());
    wait(
        &StorageFolder::GetFolderFromPathAsync(&native).unwrap(),
        deadline,
    )
    .unwrap()
    .cast()
    .unwrap()
}

fn short_path(path: &Path) -> Option<PathBuf> {
    resolve_win32_path(path, |input, output| {
        // SAFETY: `input` is NUL-terminated; `output` is a valid mutable slice when `Some`.
        unsafe { GetShortPathNameW(input, output) }
    })
}

fn create_junction(link: &Path, target: &Path) {
    let output = Command::new("cmd")
        .args(["/C", "mklink", "/J"])
        .arg(link)
        .arg(target)
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "mklink /J failed for {link:?} -> {target:?}: {}",
        String::from_utf8_lossy(&output.stderr)
    );
}

#[test]
fn durable_items_acquire_independently_and_transient_or_ephemeral_items_are_rejected() {
    let _apartment = super::super::handles::Apartment::new(RO_INIT_MULTITHREADED).unwrap();
    let fixture = Fixture(
        std::env::temp_dir().join(format!("windshare-provenance-{}", crate::IntentId::new())),
    );
    let durable_dir = fixture.0.join("durable");
    let ephemeral_dir = fixture.0.join("ephemeral");
    std::fs::create_dir_all(&durable_dir).unwrap();
    std::fs::create_dir_all(&ephemeral_dir).unwrap();

    let file_path = durable_dir.join("source.txt");
    let folder_path = durable_dir.join("folder");
    let scratch_file = ephemeral_dir.join("scratch.txt");
    std::fs::write(&file_path, b"independent local storage").unwrap();
    std::fs::create_dir(&folder_path).unwrap();
    std::fs::write(&scratch_file, b"temporary export").unwrap();

    let provenance = Provenance::from_ephemeral_roots(vec![ephemeral_dir]);
    let deadline = Instant::now() + Duration::from_secs(5);

    let file = open_file(&file_path, deadline);
    let folder = open_folder(&folder_path, deadline);
    let acquired_file = provenance.acquire(&file, deadline).unwrap();
    let acquired_folder = provenance.acquire(&folder, deadline).unwrap();
    assert_eq!(acquired_file, long_path(&file_path).unwrap());
    assert_eq!(acquired_folder, long_path(&folder_path).unwrap());
    drop(file);
    drop(folder);
    assert_eq!(
        std::fs::read(acquired_file).unwrap(),
        b"independent local storage"
    );
    assert!(std::fs::read_dir(acquired_folder).unwrap().next().is_none());

    let scratch_item = open_file(&scratch_file, deadline);
    let ephemeral_error = provenance.acquire(&scratch_item, deadline).unwrap_err();
    assert!(ephemeral_error.to_string().contains(SAVE_LOCALLY));

    let native = wide(&file_path);
    for attribute in [FILE_ATTRIBUTE_TEMPORARY, FILE_ATTRIBUTE_OFFLINE] {
        // SAFETY: only this test's existing file attribute is toggled; buffer is NUL-terminated.
        unsafe {
            SetFileAttributesW(PCWSTR(native.as_ptr()), attribute).unwrap();
        }
        let flagged = open_file(&file_path, deadline);
        let error = provenance.acquire(&flagged, deadline).unwrap_err();
        assert!(error.to_string().contains(SAVE_LOCALLY));
    }
    // SAFETY: restore the test file attribute before fixture cleanup.
    unsafe {
        SetFileAttributesW(PCWSTR(native.as_ptr()), FILE_ATTRIBUTE_NORMAL).unwrap();
    }
}

#[test]
fn ancestor_junctions_follow_target_durability() {
    let _apartment = super::super::handles::Apartment::new(RO_INIT_MULTITHREADED).unwrap();
    let fixture = Fixture(
        std::env::temp_dir().join(format!("windshare-provenance-{}", crate::IntentId::new())),
    );
    let durable_target = fixture.0.join("durable-target");
    let ephemeral_target = fixture.0.join("ephemeral-target");
    std::fs::create_dir_all(&durable_target).unwrap();
    std::fs::create_dir_all(&ephemeral_target).unwrap();

    let durable_file = durable_target.join("report.txt");
    let ephemeral_file = ephemeral_target.join("cache.txt");
    std::fs::write(&durable_file, b"mounted project data").unwrap();
    std::fs::write(&ephemeral_file, b"ephemeral via junction").unwrap();

    let durable_junction = fixture.0.join("mounted-workspace");
    let ephemeral_junction = fixture.0.join("sneaky-temp-alias");
    create_junction(&durable_junction, &durable_target);
    create_junction(&ephemeral_junction, &ephemeral_target);

    let provenance = Provenance::from_ephemeral_roots(vec![ephemeral_target]);
    let deadline = Instant::now() + Duration::from_secs(5);

    let via_durable_junction = durable_junction.join("report.txt");
    let accepted = provenance
        .acquire(&open_file(&via_durable_junction, deadline), deadline)
        .unwrap();
    assert_eq!(accepted, long_path(&via_durable_junction).unwrap());

    let via_ephemeral_junction = ephemeral_junction.join("cache.txt");
    let rejected = provenance
        .acquire(&open_file(&via_ephemeral_junction, deadline), deadline)
        .unwrap_err();
    assert!(rejected.to_string().contains(SAVE_LOCALLY));

    let _ = std::fs::remove_dir(&durable_junction);
    let _ = std::fs::remove_dir(&ephemeral_junction);
}

#[test]
fn default_provenance_rejects_system_temp_and_accepts_project_directories() {
    let _apartment = super::super::handles::Apartment::new(RO_INIT_MULTITHREADED).unwrap();
    let provenance = Provenance::new();
    let deadline = Instant::now() + Duration::from_secs(5);

    let fixture = Fixture(
        std::env::temp_dir().join(format!("windshare-provenance-{}", crate::IntentId::new())),
    );
    std::fs::create_dir_all(&fixture.0).unwrap();
    let temp_file = fixture.0.join("ephemeral.txt");
    std::fs::write(&temp_file, b"in temp dir").unwrap();
    let temp_error = provenance
        .acquire(&open_file(&temp_file, deadline), deadline)
        .unwrap_err();
    assert!(temp_error.to_string().contains(SAVE_LOCALLY));

    // Verify a real project file outside the 6 Windows Known Folders is accepted
    // (skip only if the workspace itself was placed under an ephemeral directory).
    let manifest = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("Cargo.toml");
    let canonical_manifest = std::fs::canonicalize(&manifest).unwrap();
    if !provenance.is_ephemeral(&canonical_manifest) {
        let acquired = provenance
            .acquire(&open_file(&manifest, deadline), deadline)
            .unwrap();
        assert_eq!(acquired, long_path(&manifest).unwrap());
    }
}

#[test]
fn short_path_aliases_expand_durable_items_and_still_catch_ephemeral_roots() {
    let _apartment = super::super::handles::Apartment::new(RO_INIT_MULTITHREADED).unwrap();
    let fixture = Fixture(
        std::env::temp_dir().join(format!("windshare-provenance-{}", crate::IntentId::new())),
    );
    let durable_dir = fixture.0.join("long-durable-directory");
    let ephemeral_dir = fixture.0.join("long-ephemeral-directory");
    std::fs::create_dir_all(&durable_dir).unwrap();
    std::fs::create_dir_all(&ephemeral_dir).unwrap();
    let durable_file = durable_dir.join("long-source-file.txt");
    let ephemeral_file = ephemeral_dir.join("long-temp-file.txt");
    std::fs::write(&durable_file, b"short alias durable").unwrap();
    std::fs::write(&ephemeral_file, b"short alias ephemeral").unwrap();

    assert!(long_path(&durable_dir.join("..")).is_none());
    assert!(long_path(Path::new(r"relative\source.txt")).is_none());

    let canonical_durable = long_path(&durable_file).unwrap();
    let short_durable = short_path(&canonical_durable).unwrap();
    let short_ephemeral_root = short_path(&ephemeral_dir).unwrap();
    let short_ephemeral_file = short_path(&ephemeral_file).unwrap();
    if short_durable == canonical_durable && short_ephemeral_file == ephemeral_file {
        return;
    }

    let deadline = Instant::now() + Duration::from_secs(5);
    let provenance = Provenance::from_ephemeral_roots(vec![short_ephemeral_root]);
    assert_eq!(
        provenance
            .acquire(&open_file(&short_durable, deadline), deadline)
            .unwrap(),
        canonical_durable
    );
    let error = provenance
        .acquire(&open_file(&short_ephemeral_file, deadline), deadline)
        .unwrap_err();
    assert!(error.to_string().contains(SAVE_LOCALLY));
}
