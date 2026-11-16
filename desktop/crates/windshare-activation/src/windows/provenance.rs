use super::{handles::wide, share_target::wait};
use std::{
    ffi::OsString,
    os::windows::ffi::{OsStrExt, OsStringExt},
    path::{Component, Path, PathBuf},
    time::Instant,
};
use windows::{
    Storage::{IStorageItem, StorageFile, StorageFolder, StorageItemTypes},
    Win32::{
        Storage::FileSystem::{
            FILE_ATTRIBUTE_REPARSE_POINT, FILE_ATTRIBUTE_TEMPORARY, GetDriveTypeW,
            GetFileAttributesW, GetLongPathNameW, INVALID_FILE_ATTRIBUTES,
        },
        System::Com::CoTaskMemFree,
        UI::Shell::{
            FOLDERID_Desktop, FOLDERID_Documents, FOLDERID_Downloads, FOLDERID_Music,
            FOLDERID_Pictures, FOLDERID_Videos, KF_FLAG_DEFAULT, SHGetKnownFolderPath,
        },
    },
    core::{Interface, PCWSTR},
};

#[cfg(test)]
#[path = "provenance_tests.rs"]
mod tests;

const LOCAL_PROVIDER: &str = "computer";
const DRIVE_FIXED: u32 = 3;
pub const SAVE_LOCALLY: &str = "Save these items to a local Desktop, Documents, Downloads, Pictures, Music or Videos folder, then share them again. You can also select local files directly in Explorer.";

pub struct Provenance {
    durable_roots: Vec<PathBuf>,
}
impl Provenance {
    pub fn new() -> Self {
        let roots = [
            FOLDERID_Desktop,
            FOLDERID_Documents,
            FOLDERID_Downloads,
            FOLDERID_Music,
            FOLDERID_Pictures,
            FOLDERID_Videos,
        ]
        .iter()
        .filter_map(|folder| {
            // SAFETY: known-folder GUID is valid; shell-allocated text is copied then freed.
            unsafe {
                let text = SHGetKnownFolderPath(folder, KF_FLAG_DEFAULT, None).ok()?;
                let path = PathBuf::from(OsString::from_wide(text.as_wide()));
                CoTaskMemFree(Some(text.0.cast()));
                Some(path)
            }
        })
        .collect();
        Self::from_roots(roots)
    }

    fn from_roots(roots: Vec<PathBuf>) -> Self {
        // Canonicalize 8.3 short aliases once so shell-resolved item paths match known roots.
        let durable_roots = roots
            .into_iter()
            .map(|root| long_path(&root).unwrap_or(root))
            .collect();
        Self { durable_roots }
    }

    pub fn acquire(&self, item: &IStorageItem, deadline: Instant) -> anyhow::Result<PathBuf> {
        let native_path = item.Path()?;
        let raw_path = PathBuf::from(OsString::from_wide(&native_path));
        let path = long_path(&raw_path).ok_or_else(|| anyhow::anyhow!(SAVE_LOCALLY))?;
        anyhow::ensure!(
            self.durable_roots.iter().any(|root| beneath(&path, root)),
            "{SAVE_LOCALLY}"
        );
        validate_local_chain(&path)?;
        // Provider identity plus a durable user location establishes provenance.
        // Reopening and identity comparison additionally reject supplied stream proxies.
        let (provider, same) = if item.IsOfType(StorageItemTypes::File)? {
            let file: StorageFile = item.cast()?;
            let reopened = wait(&StorageFile::GetFileFromPathAsync(&native_path)?, deadline)?;
            (file.Provider()?.Id()?, file.IsEqual(&reopened)?)
        } else if item.IsOfType(StorageItemTypes::Folder)? {
            let folder: StorageFolder = item.cast()?;
            let reopened = wait(
                &StorageFolder::GetFolderFromPathAsync(&native_path)?,
                deadline,
            )?;
            (folder.Provider()?.Id()?, folder.IsEqual(&reopened)?)
        } else {
            anyhow::bail!("{SAVE_LOCALLY}");
        };
        tracing::info!(event = "activation.source_provenance", path = ?path, provider = %provider, same);
        anyhow::ensure!(provider == LOCAL_PROVIDER && same, "{SAVE_LOCALLY}");
        Ok(path)
    }
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

/// Expands 8.3 short components without resolving reparse points (`std::fs::canonicalize`
/// follows junctions, which must remain intact for `validate_local_chain`).
fn long_path(path: &Path) -> Option<PathBuf> {
    if !is_direct_absolute(path) {
        return None;
    }
    resolve_win32_path(path, |input, output| {
        // SAFETY: `input` is NUL-terminated; `output` is a valid mutable slice when `Some`.
        unsafe { GetLongPathNameW(input, output) }
    })
}

fn beneath(path: &Path, root: &Path) -> bool {
    use windows::Win32::Globalization::{CSTR_EQUAL, CompareStringOrdinal};
    if !is_direct_absolute(path) || !is_direct_absolute(root) {
        return false;
    }
    let mut parts = path.components();
    root.is_absolute()
        && root.components().all(|expected| {
            let Some(actual) = parts.next() else {
                return false;
            };
            let actual: Vec<u16> = actual.as_os_str().encode_wide().collect();
            let expected: Vec<u16> = expected.as_os_str().encode_wide().collect();
            // SAFETY: both slices retain their native UTF-16 units for this comparison.
            unsafe { CompareStringOrdinal(&actual, &expected, true) == CSTR_EQUAL }
        })
}

fn validate_local_chain(path: &Path) -> anyhow::Result<()> {
    let root = path
        .ancestors()
        .last()
        .ok_or_else(|| anyhow::anyhow!(SAVE_LOCALLY))?;
    let drive = wide(root);
    // SAFETY: terminated drive-root path; the API does not retain the buffer.
    let drive_type = unsafe { GetDriveTypeW(PCWSTR(drive.as_ptr())) };
    anyhow::ensure!(drive_type == DRIVE_FIXED, "{SAVE_LOCALLY}");
    for ancestor in path.ancestors() {
        let native = wide(ancestor);
        // SAFETY: terminated native path; only root/ancestor metadata is queried.
        let attributes = unsafe { GetFileAttributesW(PCWSTR(native.as_ptr())) };
        anyhow::ensure!(
            attributes != INVALID_FILE_ATTRIBUTES
                && attributes & (FILE_ATTRIBUTE_REPARSE_POINT | FILE_ATTRIBUTE_TEMPORARY).0 == 0,
            "{SAVE_LOCALLY}"
        );
    }
    Ok(())
}
