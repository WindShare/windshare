use super::{handles::wide, share_target::wait};
use std::{
    ffi::OsString,
    os::windows::ffi::{OsStrExt, OsStringExt},
    path::{Component, Path, PathBuf, Prefix},
    time::Instant,
};
use windows::{
    Storage::{IStorageItem, StorageFile, StorageFolder, StorageItemTypes},
    Win32::{
        Globalization::{CSTR_EQUAL, CompareStringOrdinal},
        Storage::FileSystem::{
            FILE_ATTRIBUTE_OFFLINE, FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS, FILE_ATTRIBUTE_TEMPORARY,
            GetFileAttributesW, GetLongPathNameW, INVALID_FILE_ATTRIBUTES,
        },
        System::Com::CoTaskMemFree,
        UI::Shell::{
            FOLDERID_InternetCache, FOLDERID_LocalAppData, FOLDERID_LocalAppDataLow,
            FOLDERID_Windows, KF_FLAG_DEFAULT, SHGetKnownFolderPath,
        },
    },
    core::{GUID, HSTRING, Interface, PCWSTR},
};

#[cfg(test)]
#[path = "provenance_tests.rs"]
mod tests;

// Bit 0x00040000 is FILE_ATTRIBUTE_EA on GetFileAttributesW and must not be
// treated as FILE_ATTRIBUTE_RECALL_ON_OPEN (which applies only to dir listings).
// Match osfs by classifying FILE_ATTRIBUTE_OFFLINE as a cloud/offline
// placeholder rather than an ephemeral export so users are not told to re-pick
// the same item in Explorer.
const CLOUD_PLACEHOLDER_ATTRIBUTES: u32 =
    FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS.0 | FILE_ATTRIBUTE_OFFLINE.0;
const EPHEMERAL_ATTRIBUTE: u32 = FILE_ATTRIBUTE_TEMPORARY.0;

pub const SAVE_LOCALLY: &str = "Save these items to a regular local folder first, then share them again. You can also select files directly in Explorer.";
pub const CLOUD_PLACEHOLDER: &str = "This item isn’t fully downloaded on this device. Make it available on this device, then share it again.";

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct AcquisitionContext {
    pub intent_id: crate::IntentId,
    pub item_index: usize,
    pub item_count: usize,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum AttributeDurability {
    Durable,
    CloudPlaceholder,
    NonDurable,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum EphemeralRootKind {
    Temp,
    LocalAppDataTemp,
    AppPackages,
    LocalLowTemp,
    WindowsTemp,
    InternetCache,
    #[cfg(test)]
    Custom,
}

impl EphemeralRootKind {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Temp => "temp",
            Self::LocalAppDataTemp => "local_app_data_temp",
            Self::AppPackages => "app_packages",
            Self::LocalLowTemp => "local_low_temp",
            Self::WindowsTemp => "windows_temp",
            Self::InternetCache => "internet_cache",
            #[cfg(test)]
            Self::Custom => "custom",
        }
    }
}

struct EphemeralRoot {
    kind: EphemeralRootKind,
    path: PathBuf,
}

#[derive(Clone, Copy, Debug)]
enum RejectionDetail<'a> {
    None,
    Attributes(u32),
    IoErrorKind(std::io::ErrorKind),
    EphemeralRoot(EphemeralRootKind),
    Provider(Option<&'a str>),
}

pub struct Provenance {
    ephemeral_roots: Vec<EphemeralRoot>,
}

impl Provenance {
    pub fn new() -> Self {
        Self::from_ephemeral_roots(default_ephemeral_roots())
    }

    fn from_ephemeral_roots(roots: Vec<(EphemeralRootKind, PathBuf)>) -> Self {
        // Record both the canonical target and the un-resolved long verbatim path
        // of each ephemeral root so containment catches junctions entering a
        // scratch directory as well as ephemeral aliases pointing out to durable storage.
        let mut ephemeral_roots: Vec<EphemeralRoot> = Vec::with_capacity(roots.len());
        for (kind, root) in roots {
            for normalized in normalize_ephemeral_root(&root).into_iter().flatten() {
                if !ephemeral_roots
                    .iter()
                    .any(|existing| same_path(&existing.path, &normalized))
                {
                    ephemeral_roots.push(EphemeralRoot {
                        kind,
                        path: normalized,
                    });
                }
            }
        }
        Self { ephemeral_roots }
    }

    pub fn acquire(
        &self,
        item: &IStorageItem,
        deadline: Instant,
        context: AcquisitionContext,
    ) -> anyhow::Result<PathBuf> {
        let native_path = item.Path()?;
        let raw_path = PathBuf::from(OsString::from_wide(&native_path));
        if !is_direct_absolute(&raw_path) {
            return Err(reject(
                context,
                "non_filesystem_path",
                RejectionDetail::None,
                &raw_path,
                None,
            ));
        }
        // Inspect attributes before opening or canonicalizing so offline cloud
        // placeholders are rejected without triggering on-demand hydration.
        check_durable_path(
            context,
            &raw_path,
            None,
            "cloud_placeholder",
            "non_durable_attributes",
        )?;
        let canonical = std::fs::canonicalize(&raw_path).map_err(|error| {
            reject(
                context,
                "canonicalize_failed",
                RejectionDetail::IoErrorKind(error.kind()),
                &raw_path,
                None,
            )
        })?;
        check_durable_path(
            context,
            &raw_path,
            Some(&canonical),
            "cloud_placeholder_target",
            "non_durable_target_attributes",
        )?;
        // Expand 8.3 short aliases for clean display names while leaving any leaf
        // symlink intact so Go osfs enforces one uniform no-follow boundary.
        let handoff = long_path(&raw_path).unwrap_or_else(|| raw_path.clone());
        if let Some(root) = self.ephemeral_kind(&canonical) {
            return Err(reject(
                context,
                "ephemeral_location",
                RejectionDetail::EphemeralRoot(root),
                &handoff,
                Some(&canonical),
            ));
        }
        // Even when the resolved target is durable, handing off an alias under an
        // ephemeral root binds the share (and later repeat shares) to a transient path.
        if let Some(root) = self.ephemeral_handoff_kind(&handoff) {
            return Err(reject(
                context,
                "ephemeral_handoff_path",
                RejectionDetail::EphemeralRoot(root),
                &handoff,
                Some(&canonical),
            ));
        }
        // Reopening by filesystem path and comparing WinRT item identity rejects
        // virtual/streamed proxies that depend on the source app's lifetime.
        let Some((provider, same)) = verify_independent_item(item, &native_path, deadline)? else {
            return Err(reject(
                context,
                "unsupported_item_type",
                RejectionDetail::None,
                &handoff,
                Some(&canonical),
            ));
        };
        // Default support logs stay path-free to match host/engine support-log privacy rules;
        // explicit debug logging (RUST_LOG) retains concrete paths for local diagnosis.
        tracing::info!(
            event = "activation.source_provenance",
            intent_id = %context.intent_id,
            item_index = context.item_index,
            item_count = context.item_count,
            provider = provider.as_deref(),
            same,
        );
        tracing::debug!(
            event = "activation.source_provenance_paths",
            intent_id = %context.intent_id,
            item_index = context.item_index,
            path = ?handoff,
            canonical = ?canonical,
        );
        if !same {
            return Err(reject(
                context,
                "reopen_identity_mismatch",
                RejectionDetail::Provider(provider.as_deref()),
                &handoff,
                Some(&canonical),
            ));
        }
        Ok(handoff)
    }

    fn ephemeral_kind(&self, verbatim_path: &Path) -> Option<EphemeralRootKind> {
        self.ephemeral_roots
            .iter()
            .find(|root| beneath(verbatim_path, &root.path))
            .map(|root| root.kind)
    }

    fn ephemeral_handoff_kind(&self, handoff: &Path) -> Option<EphemeralRootKind> {
        verbatim_dos_path(handoff).and_then(|verbatim| self.ephemeral_kind(&verbatim))
    }
}

fn normalize_ephemeral_root(root: &Path) -> [Option<PathBuf>; 2] {
    let lexical = || verbatim_dos_path(&long_path(root).unwrap_or_else(|| root.to_path_buf()));
    match std::fs::canonicalize(root) {
        Ok(canonical) => [Some(canonical), lexical()],
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => [None, None],
        // Restricted system scratch directories (e.g. C:\Windows\Temp under strict ACLs)
        // must remain blocked even if canonicalize cannot open a directory handle.
        Err(_) => [lexical(), None],
    }
}

fn verbatim_dos_path(path: &Path) -> Option<PathBuf> {
    if !is_direct_absolute(path) {
        return None;
    }
    let mut components = path.components();
    let Component::Prefix(prefix) = components.next()? else {
        return None;
    };
    match prefix.kind() {
        Prefix::VerbatimDisk(_) => Some(path.to_path_buf()),
        Prefix::Disk(drive) => {
            let mut verbatim = PathBuf::from(format!(r"\\?\{}:\", char::from(drive)));
            for component in components {
                match component {
                    Component::RootDir | Component::CurDir => {}
                    Component::Normal(part) => verbatim.push(part),
                    Component::Prefix(_) | Component::ParentDir => return None,
                }
            }
            Some(verbatim)
        }
        _ => None,
    }
}

fn default_ephemeral_roots() -> Vec<(EphemeralRootKind, PathBuf)> {
    let mut roots = vec![(EphemeralRootKind::Temp, std::env::temp_dir())];
    if let Some(local_app_data) = known_folder(&FOLDERID_LocalAppData) {
        roots.push((
            EphemeralRootKind::LocalAppDataTemp,
            local_app_data.join("Temp"),
        ));
        // Packaged apps write transient share exports under TempState/AC/LocalCache;
        // their sandbox roots are app-managed rather than user-owned file locations.
        roots.push((
            EphemeralRootKind::AppPackages,
            local_app_data.join("Packages"),
        ));
    }
    if let Some(local_low) = known_folder(&FOLDERID_LocalAppDataLow) {
        roots.push((EphemeralRootKind::LocalLowTemp, local_low.join("Temp")));
    }
    if let Some(windows_dir) = known_folder(&FOLDERID_Windows) {
        roots.push((EphemeralRootKind::WindowsTemp, windows_dir.join("Temp")));
    }
    if let Some(internet_cache) = known_folder(&FOLDERID_InternetCache) {
        roots.push((EphemeralRootKind::InternetCache, internet_cache));
    }
    roots
}

fn known_folder(folder: &GUID) -> Option<PathBuf> {
    // SAFETY: known-folder GUID is valid; shell-allocated text is copied then freed.
    unsafe {
        let text = SHGetKnownFolderPath(folder, KF_FLAG_DEFAULT, None).ok()?;
        let path = PathBuf::from(OsString::from_wide(text.as_wide()));
        CoTaskMemFree(Some(text.0.cast()));
        Some(path)
    }
}

fn verify_independent_item(
    item: &IStorageItem,
    native_path: &HSTRING,
    deadline: Instant,
) -> anyhow::Result<Option<(Option<String>, bool)>> {
    if item.IsOfType(StorageItemTypes::File)? {
        let file: StorageFile = item.cast()?;
        let reopened = wait(&StorageFile::GetFileFromPathAsync(native_path)?, deadline)?;
        let provider = file
            .Provider()
            .and_then(|provider| provider.Id())
            .ok()
            .map(|id| id.to_string_lossy());
        Ok(Some((provider, file.IsEqual(&reopened)?)))
    } else if item.IsOfType(StorageItemTypes::Folder)? {
        let folder: StorageFolder = item.cast()?;
        let reopened = wait(
            &StorageFolder::GetFolderFromPathAsync(native_path)?,
            deadline,
        )?;
        let provider = folder
            .Provider()
            .and_then(|provider| provider.Id())
            .ok()
            .map(|id| id.to_string_lossy());
        Ok(Some((provider, folder.IsEqual(&reopened)?)))
    } else {
        Ok(None)
    }
}

fn check_durable_path(
    context: AcquisitionContext,
    raw_path: &Path,
    canonical: Option<&Path>,
    cloud_reason: &'static str,
    non_durable_reason: &'static str,
) -> anyhow::Result<()> {
    let checked = canonical.unwrap_or(raw_path);
    let (durability, attributes) = check_attribute_durability(checked);
    match durability {
        AttributeDurability::Durable => Ok(()),
        AttributeDurability::CloudPlaceholder => Err(reject_cloud_placeholder(
            context,
            cloud_reason,
            RejectionDetail::Attributes(attributes),
            raw_path,
            canonical,
        )),
        AttributeDurability::NonDurable => Err(reject(
            context,
            non_durable_reason,
            RejectionDetail::Attributes(attributes),
            raw_path,
            canonical,
        )),
    }
}

fn reject(
    context: AcquisitionContext,
    reason: &'static str,
    detail: RejectionDetail<'_>,
    path: &Path,
    canonical: Option<&Path>,
) -> anyhow::Error {
    reject_with(context, reason, detail, path, canonical, SAVE_LOCALLY)
}

fn reject_cloud_placeholder(
    context: AcquisitionContext,
    reason: &'static str,
    detail: RejectionDetail<'_>,
    path: &Path,
    canonical: Option<&Path>,
) -> anyhow::Error {
    reject_with(context, reason, detail, path, canonical, CLOUD_PLACEHOLDER)
}

fn reject_with(
    context: AcquisitionContext,
    reason: &'static str,
    detail: RejectionDetail<'_>,
    path: &Path,
    canonical: Option<&Path>,
    message: &'static str,
) -> anyhow::Error {
    let (attributes, error_kind, ephemeral_root, provider) = match detail {
        RejectionDetail::None => (None, None, None, None),
        RejectionDetail::Attributes(bits) => (Some(format!("{bits:#010x}")), None, None, None),
        RejectionDetail::IoErrorKind(kind) => (None, Some(kind), None, None),
        RejectionDetail::EphemeralRoot(root) => (None, None, Some(root.as_str()), None),
        RejectionDetail::Provider(provider) => (None, None, None, provider),
    };
    // Default support logs keep rejection reasons and correlation fields only;
    // raw filesystem paths stay behind explicit debug tracing (RUST_LOG).
    tracing::warn!(
        event = "activation.source_provenance_rejected",
        intent_id = %context.intent_id,
        item_index = context.item_index,
        item_count = context.item_count,
        reason,
        attributes = attributes.as_deref(),
        error_kind = error_kind.map(tracing::field::debug),
        ephemeral_root,
        provider,
    );
    tracing::debug!(
        event = "activation.source_provenance_rejected_paths",
        intent_id = %context.intent_id,
        item_index = context.item_index,
        reason,
        path = ?path,
        canonical = ?canonical,
    );
    anyhow::anyhow!(message)
}

fn check_attribute_durability(path: &Path) -> (AttributeDurability, u32) {
    let native = wide(path);
    // SAFETY: terminated native path; only file metadata attributes are queried.
    let attributes = unsafe { GetFileAttributesW(PCWSTR(native.as_ptr())) };
    (classify_get_file_attributes(attributes), attributes)
}

const fn classify_get_file_attributes(attributes: u32) -> AttributeDurability {
    if attributes == INVALID_FILE_ATTRIBUTES {
        return AttributeDurability::NonDurable;
    }
    if attributes & CLOUD_PLACEHOLDER_ATTRIBUTES != 0 {
        return AttributeDurability::CloudPlaceholder;
    }
    if attributes & EPHEMERAL_ATTRIBUTE != 0 {
        return AttributeDurability::NonDurable;
    }
    AttributeDurability::Durable
}

fn is_direct_absolute(path: &Path) -> bool {
    path.is_absolute()
        && !path
            .components()
            .any(|part| matches!(part, Component::ParentDir))
}

fn resolve_win32_path(
    path: &Path,
    query: impl Fn(PCWSTR, Option<&mut [u16]>) -> u32,
) -> Option<PathBuf> {
    let input = wide(path);
    let mut capacity = 0usize;
    for _ in 0..4 {
        let mut buffer = vec![0u16; capacity];
        let output = (!buffer.is_empty()).then_some(buffer.as_mut_slice());
        let result = query(PCWSTR(input.as_ptr()), output) as usize;
        if result == 0 {
            return None;
        }
        if result < capacity {
            buffer.truncate(result);
            return Some(PathBuf::from(OsString::from_wide(&buffer)));
        }
        capacity = result;
    }
    None
}

/// Expands 8.3 short components without resolving reparse points.
fn long_path(path: &Path) -> Option<PathBuf> {
    if !is_direct_absolute(path) {
        return None;
    }
    resolve_win32_path(path, |input, output| {
        // SAFETY: `input` is NUL-terminated; `output` is a valid mutable slice when `Some`.
        unsafe { GetLongPathNameW(input, output) }
    })
}

fn same_path(left: &Path, right: &Path) -> bool {
    beneath(left, right) && beneath(right, left)
}

fn beneath(path: &Path, root: &Path) -> bool {
    if !is_direct_absolute(path) || !is_direct_absolute(root) {
        return false;
    }
    let mut parts = path.components();
    root.components().all(|expected| {
        let Some(actual) = parts.next() else {
            return false;
        };
        let actual: Vec<u16> = actual.as_os_str().encode_wide().collect();
        let expected: Vec<u16> = expected.as_os_str().encode_wide().collect();
        // SAFETY: both slices retain their native UTF-16 units for this comparison.
        unsafe { CompareStringOrdinal(&actual, &expected, true) == CSTR_EQUAL }
    })
}
