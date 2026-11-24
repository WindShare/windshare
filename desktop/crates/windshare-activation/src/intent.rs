use serde::{Deserialize, Serialize};
use std::path::PathBuf;
use uuid::Uuid;

// Matches core/catalog.MaxSelectedRoots; the host remains the filesystem authority.
pub const MAX_SELECTED_ROOTS: usize = 4_096;

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq, Hash)]
#[serde(transparent)]
pub struct IntentId(Uuid);

impl Default for IntentId {
    fn default() -> Self {
        Self::new()
    }
}

impl IntentId {
    pub fn new() -> Self {
        Self(Uuid::new_v4())
    }
}

impl std::fmt::Display for IntentId {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        self.0.fmt(formatter)
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum EntrySource {
    CommandLine,
    Explorer,
    ShareTarget,
    Picker,
    Drop,
    Repeat,
}

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct ShareIntent {
    pub id: IntentId,
    pub source: EntrySource,
    #[serde(with = "native_paths")]
    pub roots: Vec<PathBuf>,
}

impl ShareIntent {
    pub fn new(source: EntrySource, roots: Vec<PathBuf>) -> Self {
        Self::with_id(IntentId::new(), source, roots)
    }

    pub const fn with_id(id: IntentId, source: EntrySource, roots: Vec<PathBuf>) -> Self {
        Self { id, source, roots }
    }

    /// # Errors
    /// Rejects an empty, excessive, relative, or empty-path selection.
    pub fn validate(&self) -> Result<(), String> {
        if self.roots.is_empty() {
            return Err("Select at least one file or folder".into());
        }
        if self.roots.len() > MAX_SELECTED_ROOTS {
            return Err("Too many selected paths; share their containing folder instead".into());
        }
        if self.roots.iter().any(|path| !path.is_absolute()) {
            return Err("Selected paths must be absolute".into());
        }
        Ok(())
    }
}

// IPC must not replace unpaired UTF-16 surrogates before the host can explicitly
// reject an unsupported path. JSON strings and PathBuf's serde impl cannot do this.
mod native_paths {
    use serde::{Deserialize, Deserializer, Serialize, Serializer};
    use std::{ffi::OsString, path::PathBuf};

    #[cfg(windows)]
    type Unit = u16;
    #[cfg(not(windows))]
    type Unit = u8;

    pub fn serialize<S: Serializer>(paths: &[PathBuf], serializer: S) -> Result<S::Ok, S::Error> {
        let encoded: Vec<Vec<Unit>> = paths
            .iter()
            .map(|path| {
                #[cfg(windows)]
                {
                    use std::os::windows::ffi::OsStrExt;
                    path.as_os_str().encode_wide().collect()
                }
                #[cfg(not(windows))]
                {
                    use std::os::unix::ffi::OsStrExt;
                    path.as_os_str().as_bytes().to_vec()
                }
            })
            .collect();
        encoded.serialize(serializer)
    }

    pub fn deserialize<'de, D: Deserializer<'de>>(
        deserializer: D,
    ) -> Result<Vec<PathBuf>, D::Error> {
        Vec::<Vec<Unit>>::deserialize(deserializer).map(|paths| {
            paths
                .into_iter()
                .map(|path| {
                    #[cfg(windows)]
                    {
                        use std::os::windows::ffi::OsStringExt;
                        PathBuf::from(OsString::from_wide(&path))
                    }
                    #[cfg(not(windows))]
                    {
                        use std::os::unix::ffi::OsStringExt;
                        PathBuf::from(OsString::from_vec(path))
                    }
                })
                .collect()
        })
    }
}
