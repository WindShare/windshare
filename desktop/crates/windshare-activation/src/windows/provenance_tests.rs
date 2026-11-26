use super::*;
use std::{
    cell::RefCell, collections::BTreeMap, fmt, process::Command, sync::Once, time::Duration,
};
use windows::Win32::{
    Storage::FileSystem::{FILE_ATTRIBUTE_NORMAL, GetShortPathNameW, SetFileAttributesW},
    System::WinRT::RO_INIT_MULTITHREADED,
};

fn test_context() -> AcquisitionContext {
    AcquisitionContext {
        intent_id: crate::IntentId::new(),
        item_index: 0,
        item_count: 1,
    }
}

fn custom_ephemeral_roots(roots: Vec<PathBuf>) -> Provenance {
    Provenance::from_ephemeral_roots(
        roots
            .into_iter()
            .map(|path| (EphemeralRootKind::Custom, path))
            .collect(),
    )
}

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
        verbatim_dos_path(Path::new("c:/Windows/./Temp/")),
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

struct Junction(PathBuf);
impl Drop for Junction {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir(&self.0);
    }
}

fn create_junction(link: PathBuf, target: &Path) -> Junction {
    let output = Command::new("cmd")
        .args(["/C", "mklink", "/J"])
        .arg(&link)
        .arg(target)
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "mklink /J failed for {link:?} -> {target:?}: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    Junction(link)
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

    let provenance = custom_ephemeral_roots(vec![ephemeral_dir]);
    let deadline = Instant::now() + Duration::from_secs(5);
    let context = test_context();

    let file = open_file(&file_path, deadline);
    let folder = open_folder(&folder_path, deadline);
    let acquired_file = provenance.acquire(&file, deadline, context).unwrap();
    let acquired_folder = provenance.acquire(&folder, deadline, context).unwrap();
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
    let ephemeral_error = provenance
        .acquire(&scratch_item, deadline, context)
        .unwrap_err();
    assert!(ephemeral_error.to_string().contains(SAVE_LOCALLY));

    let native = wide(&file_path);
    for (attribute, expected) in [
        (FILE_ATTRIBUTE_TEMPORARY, SAVE_LOCALLY),
        (FILE_ATTRIBUTE_OFFLINE, CLOUD_PLACEHOLDER),
    ] {
        // SAFETY: only this test's existing file attribute is toggled; buffer is NUL-terminated.
        unsafe {
            SetFileAttributesW(PCWSTR(native.as_ptr()), attribute).unwrap();
        }
        let flagged = open_file(&file_path, deadline);
        let error = provenance.acquire(&flagged, deadline, context).unwrap_err();
        assert!(error.to_string().contains(expected));
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

    let durable_junction = create_junction(fixture.0.join("mounted-workspace"), &durable_target);
    let ephemeral_junction =
        create_junction(fixture.0.join("sneaky-temp-alias"), &ephemeral_target);

    let provenance = custom_ephemeral_roots(vec![ephemeral_target.clone()]);
    let deadline = Instant::now() + Duration::from_secs(5);
    let context = test_context();

    let via_durable_junction = durable_junction.0.join("report.txt");
    let accepted = provenance
        .acquire(
            &open_file(&via_durable_junction, deadline),
            deadline,
            context,
        )
        .unwrap();
    assert_eq!(accepted, long_path(&via_durable_junction).unwrap());

    let via_ephemeral_junction = ephemeral_junction.0.join("cache.txt");
    let rejected = provenance
        .acquire(
            &open_file(&via_ephemeral_junction, deadline),
            deadline,
            context,
        )
        .unwrap_err();
    assert!(rejected.to_string().contains(SAVE_LOCALLY));

    let ephemeral_to_durable =
        create_junction(ephemeral_target.join("temp-to-durable"), &durable_target);
    let via_ephemeral_alias = ephemeral_to_durable.0.join("report.txt");
    let alias_rejected = provenance
        .acquire(
            &open_file(&via_ephemeral_alias, deadline),
            deadline,
            context,
        )
        .unwrap_err();
    assert!(alias_rejected.to_string().contains(SAVE_LOCALLY));

    let ephemeral_root_alias =
        create_junction(fixture.0.join("temp-root-alias"), &ephemeral_target);
    let aliased_root_provenance = custom_ephemeral_roots(vec![ephemeral_root_alias.0.clone()]);
    let via_aliased_ephemeral_root = ephemeral_root_alias
        .0
        .join("temp-to-durable")
        .join("report.txt");
    let aliased_root_rejected = aliased_root_provenance
        .acquire(
            &open_file(&via_aliased_ephemeral_root, deadline),
            deadline,
            context,
        )
        .unwrap_err();
    assert!(aliased_root_rejected.to_string().contains(SAVE_LOCALLY));
}

#[test]
fn default_provenance_rejects_system_temp_and_accepts_project_directories() {
    let _apartment = super::super::handles::Apartment::new(RO_INIT_MULTITHREADED).unwrap();
    let provenance = Provenance::new();
    let deadline = Instant::now() + Duration::from_secs(5);
    let context = test_context();

    let fixture = Fixture(
        std::env::temp_dir().join(format!("windshare-provenance-{}", crate::IntentId::new())),
    );
    std::fs::create_dir_all(&fixture.0).unwrap();
    let temp_file = fixture.0.join("ephemeral.txt");
    std::fs::write(&temp_file, b"in temp dir").unwrap();
    let temp_error = provenance
        .acquire(&open_file(&temp_file, deadline), deadline, context)
        .unwrap_err();
    assert!(temp_error.to_string().contains(SAVE_LOCALLY));

    // Verify a real project file outside the 6 Windows Known Folders is accepted
    // (skip only if the workspace itself was placed under an ephemeral directory).
    // Prefer Cargo's runtime CARGO_MANIFEST_DIR so a cached test binary reused
    // across worktrees stays valid after the compiling worktree is removed.
    let manifest_dir = std::env::var_os("CARGO_MANIFEST_DIR")
        .map_or_else(|| PathBuf::from(env!("CARGO_MANIFEST_DIR")), PathBuf::from);
    let manifest = manifest_dir.join("Cargo.toml");
    let canonical_manifest = std::fs::canonicalize(&manifest).unwrap();
    if provenance.ephemeral_kind(&canonical_manifest).is_none() {
        let acquired = provenance
            .acquire(&open_file(&manifest, deadline), deadline, context)
            .unwrap();
        assert_eq!(acquired, long_path(&manifest).unwrap());

        let temp_alias = create_junction(fixture.0.join("project-link"), &manifest_dir);
        let temp_alias_error = provenance
            .acquire(
                &open_file(&temp_alias.0.join("Cargo.toml"), deadline),
                deadline,
                context,
            )
            .unwrap_err();
        assert!(temp_alias_error.to_string().contains(SAVE_LOCALLY));
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
    let context = test_context();
    let provenance = custom_ephemeral_roots(vec![short_ephemeral_root]);
    assert_eq!(
        provenance
            .acquire(&open_file(&short_durable, deadline), deadline, context)
            .unwrap(),
        canonical_durable
    );
    let error = provenance
        .acquire(
            &open_file(&short_ephemeral_file, deadline),
            deadline,
            context,
        )
        .unwrap_err();
    assert!(error.to_string().contains(SAVE_LOCALLY));
}

#[test]
fn get_file_attributes_distinguishes_ea_ephemeral_and_cloud_placeholders() {
    use windows::Win32::Storage::FileSystem::FILE_ATTRIBUTE_EA;
    assert_eq!(
        classify_get_file_attributes(FILE_ATTRIBUTE_NORMAL.0 | FILE_ATTRIBUTE_EA.0),
        AttributeDurability::Durable
    );
    assert_eq!(
        classify_get_file_attributes(FILE_ATTRIBUTE_TEMPORARY.0),
        AttributeDurability::NonDurable
    );
    assert_eq!(
        classify_get_file_attributes(FILE_ATTRIBUTE_OFFLINE.0),
        AttributeDurability::CloudPlaceholder
    );
    assert_eq!(
        classify_get_file_attributes(FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS.0),
        AttributeDurability::CloudPlaceholder
    );
    assert_eq!(
        classify_get_file_attributes(
            FILE_ATTRIBUTE_OFFLINE.0 | FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS.0
        ),
        AttributeDurability::CloudPlaceholder
    );
    assert_eq!(
        classify_get_file_attributes(INVALID_FILE_ATTRIBUTES),
        AttributeDurability::NonDurable
    );
}

#[derive(Clone, Debug)]
struct CapturedEvent {
    level: tracing::Level,
    fields: BTreeMap<String, String>,
}

struct FieldVisitor(BTreeMap<String, String>);

impl tracing::field::Visit for FieldVisitor {
    fn record_str(&mut self, field: &tracing::field::Field, value: &str) {
        self.0.insert(field.name().to_owned(), value.to_owned());
    }

    fn record_debug(&mut self, field: &tracing::field::Field, value: &dyn fmt::Debug) {
        self.0.insert(field.name().to_owned(), format!("{value:?}"));
    }
}

thread_local! {
    static CAPTURED_EVENTS: RefCell<Option<Vec<CapturedEvent>>> = const { RefCell::new(None) };
}

struct ThreadLocalCapture;

impl tracing::Subscriber for ThreadLocalCapture {
    fn enabled(&self, _metadata: &tracing::Metadata<'_>) -> bool {
        true
    }

    fn new_span(&self, _span: &tracing::span::Attributes<'_>) -> tracing::span::Id {
        tracing::span::Id::from_u64(1)
    }

    fn record(&self, _span: &tracing::span::Id, _values: &tracing::span::Record<'_>) {}

    fn record_follows_from(&self, _span: &tracing::span::Id, _follows: &tracing::span::Id) {}

    fn event(&self, event: &tracing::Event<'_>) {
        CAPTURED_EVENTS.with(|slot| {
            let mut slot = slot.borrow_mut();
            let Some(events) = slot.as_mut() else {
                return;
            };
            let mut visitor = FieldVisitor(BTreeMap::new());
            event.record(&mut visitor);
            events.push(CapturedEvent {
                level: *event.metadata().level(),
                fields: visitor.0,
            });
        });
    }

    fn enter(&self, _span: &tracing::span::Id) {}

    fn exit(&self, _span: &tracing::span::Id) {}
}

struct CaptureGuard;

impl Drop for CaptureGuard {
    fn drop(&mut self) {
        CAPTURED_EVENTS.with(|slot| {
            *slot.borrow_mut() = None;
        });
    }
}

fn capture_events(run: impl FnOnce()) -> Vec<CapturedEvent> {
    // Install one process-wide subscriber so concurrent test threads cannot cache
    // `Interest::never()` on provenance callsites via `NoSubscriber` before capture.
    static INSTALL: Once = Once::new();
    INSTALL.call_once(|| {
        let _ = tracing::subscriber::set_global_default(ThreadLocalCapture);
    });
    tracing::callsite::rebuild_interest_cache();
    CAPTURED_EVENTS.with(|slot| {
        *slot.borrow_mut() = Some(Vec::new());
    });
    let _guard = CaptureGuard;
    run();
    CAPTURED_EVENTS.with(|slot| slot.borrow_mut().take().unwrap_or_default())
}

fn find_event<'a>(
    events: &'a [CapturedEvent],
    level: tracing::Level,
    name: &str,
    item_index: &str,
) -> &'a CapturedEvent {
    events
        .iter()
        .find(|event| {
            event.level == level
                && event.fields.get("event").map(String::as_str) == Some(name)
                && event.fields.get("item_index").map(String::as_str) == Some(item_index)
        })
        .unwrap_or_else(|| {
            panic!("missing {level:?} event {name} for item {item_index}: {events:?}")
        })
}

fn assert_default_logs_omit_paths(events: &[CapturedEvent], secrets: &[&str]) {
    for event in events.iter().filter(|event| {
        matches!(
            event.level,
            tracing::Level::ERROR | tracing::Level::WARN | tracing::Level::INFO
        )
    }) {
        assert!(
            !event.fields.contains_key("path"),
            "default log had path: {event:?}"
        );
        assert!(
            !event.fields.contains_key("canonical"),
            "default log had canonical: {event:?}"
        );
        for value in event.fields.values() {
            for secret in secrets {
                assert!(
                    !value.contains(secret),
                    "default support log leaked {secret:?} in {event:?}"
                );
            }
        }
    }
}

fn assert_expected_provenance_events(events: &[CapturedEvent], intent_id: crate::IntentId) {
    let intent_text = intent_id.to_string();
    let accepted = find_event(
        events,
        tracing::Level::INFO,
        "activation.source_provenance",
        "0",
    );
    assert_eq!(accepted.fields.get("intent_id"), Some(&intent_text));
    assert_eq!(
        accepted.fields.get("item_count").map(String::as_str),
        Some("4")
    );
    assert_eq!(
        accepted.fields.get("same").map(String::as_str),
        Some("true")
    );

    let ephemeral = find_event(
        events,
        tracing::Level::WARN,
        "activation.source_provenance_rejected",
        "1",
    );
    assert_eq!(ephemeral.fields.get("intent_id"), Some(&intent_text));
    assert_eq!(
        ephemeral.fields.get("reason").map(String::as_str),
        Some("ephemeral_location")
    );
    assert_eq!(
        ephemeral.fields.get("ephemeral_root").map(String::as_str),
        Some("app_packages")
    );

    let handoff = find_event(
        events,
        tracing::Level::WARN,
        "activation.source_provenance_rejected",
        "2",
    );
    assert_eq!(
        handoff.fields.get("reason").map(String::as_str),
        Some("ephemeral_handoff_path")
    );
    assert_eq!(
        handoff.fields.get("ephemeral_root").map(String::as_str),
        Some("app_packages")
    );

    let placeholder = find_event(
        events,
        tracing::Level::WARN,
        "activation.source_provenance_rejected",
        "3",
    );
    assert_eq!(
        placeholder.fields.get("reason").map(String::as_str),
        Some("cloud_placeholder")
    );
    let raw_attrs = placeholder.fields.get("attributes").unwrap();
    let parsed_attrs = u32::from_str_radix(raw_attrs.strip_prefix("0x").unwrap(), 16).unwrap();
    assert_ne!(parsed_attrs & FILE_ATTRIBUTE_OFFLINE.0, 0);

    let accepted_debug = find_event(
        events,
        tracing::Level::DEBUG,
        "activation.source_provenance_paths",
        "0",
    );
    assert!(accepted_debug.fields["path"].contains("private-junction-alias"));
    assert!(accepted_debug.fields["canonical"].contains("private-durable-dir"));

    let handoff_debug = find_event(
        events,
        tracing::Level::DEBUG,
        "activation.source_provenance_rejected_paths",
        "2",
    );
    assert!(handoff_debug.fields["path"].contains("private-ephemeral-alias"));
    assert!(handoff_debug.fields["canonical"].contains("private-durable-dir"));
}

#[test]
fn support_logs_omit_source_paths_while_debug_tracing_retains_them() {
    let _apartment = super::super::handles::Apartment::new(RO_INIT_MULTITHREADED).unwrap();
    let token = format!("private-root-{}", crate::IntentId::new());
    let fixture = Fixture(std::env::temp_dir().join(&token));
    let durable_dir = fixture.0.join("private-durable-dir");
    let ephemeral_dir = fixture.0.join("private-ephemeral-dir");
    std::fs::create_dir_all(&durable_dir).unwrap();
    std::fs::create_dir_all(&ephemeral_dir).unwrap();

    let durable_file = durable_dir.join("private-accepted-fixture.txt");
    let ephemeral_file = ephemeral_dir.join("private-ephemeral-fixture.txt");
    std::fs::write(&durable_file, b"accepted").unwrap();
    std::fs::write(&ephemeral_file, b"ephemeral").unwrap();

    let durable_junction = create_junction(fixture.0.join("private-junction-alias"), &durable_dir);
    let ephemeral_alias =
        create_junction(ephemeral_dir.join("private-ephemeral-alias"), &durable_dir);

    let provenance =
        Provenance::from_ephemeral_roots(vec![(EphemeralRootKind::AppPackages, ephemeral_dir)]);
    let intent_id = crate::IntentId::new();
    let deadline = Instant::now() + Duration::from_secs(5);
    let context = |item_index| AcquisitionContext {
        intent_id,
        item_index,
        item_count: 4,
    };

    let events = capture_events(|| {
        let via_junction = durable_junction.0.join("private-accepted-fixture.txt");
        provenance
            .acquire(&open_file(&via_junction, deadline), deadline, context(0))
            .unwrap();
        let _ = provenance.acquire(&open_file(&ephemeral_file, deadline), deadline, context(1));
        let via_ephemeral_alias = ephemeral_alias.0.join("private-accepted-fixture.txt");
        let _ = provenance.acquire(
            &open_file(&via_ephemeral_alias, deadline),
            deadline,
            context(2),
        );

        let native = wide(&durable_file);
        // SAFETY: only this test's existing file attribute is toggled; buffer is NUL-terminated.
        unsafe {
            SetFileAttributesW(PCWSTR(native.as_ptr()), FILE_ATTRIBUTE_OFFLINE).unwrap();
        }
        let _ = provenance.acquire(&open_file(&durable_file, deadline), deadline, context(3));
        // SAFETY: restore the test file attribute before fixture cleanup.
        unsafe {
            SetFileAttributesW(PCWSTR(native.as_ptr()), FILE_ATTRIBUTE_NORMAL).unwrap();
        }
    });

    assert_default_logs_omit_paths(
        &events,
        &[
            token.as_str(),
            "private-durable-dir",
            "private-ephemeral-dir",
            "private-accepted-fixture",
            "private-ephemeral-fixture",
            "private-junction-alias",
            "private-ephemeral-alias",
        ],
    );
    assert_expected_provenance_events(&events, intent_id);
}
