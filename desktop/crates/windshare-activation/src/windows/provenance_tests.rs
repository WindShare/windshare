use super::*;
use std::time::Duration;
use windows::Win32::{
    Storage::FileSystem::{FILE_ATTRIBUTE_NORMAL, SetFileAttributesW},
    System::WinRT::RO_INIT_MULTITHREADED,
};

#[test]
fn durable_locations_use_native_case_insensitive_components() {
    let root = Path::new(r"C:\Users\Example\Documents");
    assert!(beneath(
        Path::new(r"c:\users\example\DOCUMENTS\report.txt"),
        root
    ));
    assert!(!beneath(
        Path::new(r"C:\Users\Example\Documents-old\report.txt"),
        root
    ));
    assert!(!beneath(
        Path::new(r"C:\Users\Example\Documents\..\cache.txt"),
        root
    ));
    assert!(!beneath(Path::new(r"Documents\report.txt"), root));
}

struct Fixture(PathBuf);
impl Drop for Fixture {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

#[test]
fn local_storage_items_reopen_independently_and_temporary_items_are_rejected() {
    let _apartment = super::super::handles::Apartment::new(RO_INIT_MULTITHREADED).unwrap();
    let fixture = Fixture(
        std::env::temp_dir().join(format!("windshare-provenance-{}", crate::IntentId::new())),
    );
    std::fs::create_dir(&fixture.0).unwrap();
    let file_path = fixture.0.join("source.txt");
    let folder_path = fixture.0.join("folder");
    std::fs::write(&file_path, b"independent local storage").unwrap();
    std::fs::create_dir(&folder_path).unwrap();
    // Inject the test-owned location so the contract probe never writes user libraries.
    let provenance = Provenance {
        durable_roots: vec![fixture.0.clone()],
    };
    let deadline = Instant::now() + Duration::from_secs(5);
    let native_file = windows::core::HSTRING::from(file_path.as_os_str());
    let native_folder = windows::core::HSTRING::from(folder_path.as_os_str());
    let file = wait(
        &StorageFile::GetFileFromPathAsync(&native_file).unwrap(),
        deadline,
    )
    .unwrap();
    let folder = wait(
        &StorageFolder::GetFolderFromPathAsync(&native_folder).unwrap(),
        deadline,
    )
    .unwrap();
    let acquired_file = provenance.acquire(&file.cast().unwrap(), deadline).unwrap();
    let acquired_folder = provenance
        .acquire(&folder.cast().unwrap(), deadline)
        .unwrap();
    assert_eq!(acquired_file, file_path);
    assert_eq!(acquired_folder, folder_path);
    drop(file);
    drop(folder);
    assert_eq!(
        std::fs::read(acquired_file).unwrap(),
        b"independent local storage"
    );
    assert!(std::fs::read_dir(acquired_folder).unwrap().next().is_none());

    let native = wide(&file_path);
    // SAFETY: only this test's existing file is marked temporary; its buffer is terminated.
    unsafe {
        SetFileAttributesW(PCWSTR(native.as_ptr()), FILE_ATTRIBUTE_TEMPORARY).unwrap();
    }
    let temporary = wait(
        &StorageFile::GetFileFromPathAsync(&native_file).unwrap(),
        deadline,
    )
    .unwrap();
    let error = provenance
        .acquire(&temporary.cast().unwrap(), deadline)
        .unwrap_err();
    assert!(error.to_string().contains(SAVE_LOCALLY));
    // SAFETY: restore the same owned file before fixture cleanup.
    unsafe {
        SetFileAttributesW(PCWSTR(native.as_ptr()), FILE_ATTRIBUTE_NORMAL).unwrap();
    }
}
