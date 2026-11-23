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
enum AttributeDurability {
    Durable,
    CloudPlaceholder,
    NonDurable,
}

pub struct Provenance {
    ephemeral_roots: Vec<PathBuf>,
}

impl Provenance {
    pub fn new() -> Self {
        Self::from_ephemeral_roots(default_ephemeral_roots())
    }

    fn from_ephemeral_roots(roots: Vec<PathBuf>) -> Self {
        // Record both the canonical target and the un-resolved long verbatim path
        // of each ephemeral root so containment catches junctions entering a
        // scratch directory as well as ephemeral aliases pointing out to durable storage.
        let mut ephemeral_roots: Vec<PathBuf> = Vec::with_capacity(roots.len());
        for root in roots {
            for normalized in normalize_ephemeral_root(&root).into_iter().flatten() {
                if !ephemeral_roots
                    .iter()
                    .any(|existing| same_path(existing, &normalized))
                {
                    ephemeral_roots.push(normalized);
                }
            }
        }
        Self { ephemeral_roots }
    }

    pub fn acquire(&self, item: &IStorageItem, deadline: Instant) -> anyhow::Result<PathBuf> {
        let native_path = item.Path()?;
        let raw_path = PathBuf::from(OsString::from_wide(&native_path));
        if !is_direct_absolute(&raw_path) {
            return Err(reject("non_filesystem_path", &raw_path, None));
        }
        // Inspect attributes before opening or canonicalizing so offline cloud
        // placeholders are rejected without triggering on-demand hydration.
        match check_attribute_durability(&raw_path) {
            AttributeDurability::Durable => {}
            AttributeDurability::CloudPlaceholder => {
                return Err(reject_cloud_placeholder(
                    "cloud_placeholder",
                    &raw_path,
                    None,
                ));
            }
            AttributeDurability::NonDurable => {
                return Err(reject("non_durable_attributes", &raw_path, None));
            }
        }
        let canonical = std::fs::canonicalize(&raw_path)
            .map_err(|_| reject("canonicalize_failed", &raw_path, None))?;
        match check_attribute_durability(&canonical) {
            AttributeDurability::Durable => {}
            AttributeDurability::CloudPlaceholder => {
                return Err(reject_cloud_placeholder(
                    "cloud_placeholder_target",
                    &raw_path,
                    Some(&canonical),
                ));
            }
            AttributeDurability::NonDurable => {
                return Err(reject(
                    "non_durable_target_attributes",
                    &raw_path,
                    Some(&canonical),
                ));
            }
        }
        // Expand 8.3 short aliases for clean display names while leaving any leaf
        // symlink intact so Go osfs enforces one uniform no-follow boundary.
        let handoff = long_path(&raw_path).unwrap_or_else(|| raw_path.clone());
        if self.is_ephemeral(&canonical) {
            return Err(reject("ephemeral_location", &handoff, Some(&canonical)));
        }
        // Even when the resolved target is durable, handing off an alias under an
        // ephemeral root binds the share (and later repeat shares) to a transient path.
        if self.is_ephemeral_handoff(&handoff) {
            return Err(reject("ephemeral_handoff_path", &handoff, Some(&canonical)));
        }
        // Reopening by filesystem path and comparing WinRT item identity rejects
        // virtual/streamed proxies that depend on the source app's lifetime.
        let Some((provider, same)) = verify_independent_item(item, &native_path, deadline)? else {
            return Err(reject("unsupported_item_type", &handoff, Some(&canonical)));
        };
        tracing::info!(
            event = "activation.source_provenance",
            path = ?handoff,
            canonical = ?canonical,
            provider = ?provider,
            same
        );
        if !same {
            return Err(reject(
                "reopen_identity_mismatch",
                &handoff,
                Some(&canonical),
            ));
        }
        Ok(handoff)
    }

    fn is_ephemeral(&self, verbatim_path: &Path) -> bool {
        self.ephemeral_roots
            .iter()
            .any(|root| beneath(verbatim_path, root))
    }

    fn is_ephemeral_handoff(&self, handoff: &Path) -> bool {
        verbatim_dos_path(handoff).is_some_and(|verbatim| self.is_ephemeral(&verbatim))
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

fn default_ephemeral_roots() -> Vec<PathBuf> {
    let mut roots = vec![std::env::temp_dir()];
    if let Some(local_app_data) = known_folder(&FOLDERID_LocalAppData) {
        roots.push(local_app_data.join("Temp"));
        // Packaged apps write transient share exports under TempState/AC/LocalCache;
        // their sandbox roots are app-managed rather than user-owned file locations.
        roots.push(local_app_data.join("Packages"));
    }
    if let Some(local_low) = known_folder(&FOLDERID_LocalAppDataLow) {
        roots.push(local_low.join("Temp"));
    }
    if let Some(windows_dir) = known_folder(&FOLDERID_Windows) {
        roots.push(windows_dir.join("Temp"));
    }
    if let Some(internet_cache) = known_folder(&FOLDERID_InternetCache) {
        roots.push(internet_cache);
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

fn reject(reason: &'static str, path: &Path, canonical: Option<&Path>) -> anyhow::Error {
    reject_with(reason, path, canonical, SAVE_LOCALLY)
}

fn reject_cloud_placeholder(
    reason: &'static str,
    path: &Path,
    canonical: Option<&Path>,
) -> anyhow::Error {
    reject_with(reason, path, canonical, CLOUD_PLACEHOLDER)
}

fn reject_with(
    reason: &'static str,
    path: &Path,
    canonical: Option<&Path>,
    message: &'static str,
) -> anyhow::Error {
    tracing::warn!(
        event = "activation.source_provenance_rejected",
        reason,
        path = ?path,
        canonical = ?canonical
    );
    anyhow::anyhow!(message)
}

fn check_attribute_durability(path: &Path) -> AttributeDurability {
    let native = wide(path);
    // SAFETY: terminated native path; only file metadata attributes are queried.
    let attributes = unsafe { GetFileAttributesW(PCWSTR(native.as_ptr())) };
    classify_get_file_attributes(attributes)
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
