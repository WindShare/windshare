mod appearance;
mod view;

pub use appearance::{AppearanceSettings, select_appearance};

use std::{
    fs,
    io::{self, Write},
    path::{Path, PathBuf},
};

pub trait PreferenceChoice: Copy + Default {
    const KEY: &'static str;
    fn code(self) -> &'static str;
    fn parse(value: &str) -> Option<Self>;
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Persistence {
    Saved,
    LoadFailed,
    SaveFailed,
}

pub struct Preference<C> {
    pub choice: C,
    pub persistence: Persistence,
    path: Option<PathBuf>,
}

impl<C: PreferenceChoice> Preference<C> {
    pub fn load() -> Self {
        Self::load_from(
            dirs::config_dir()
                .map(|directory| directory.join("WindShare").join("desktop").join(C::KEY)),
        )
    }

    fn load_from(path: Option<PathBuf>) -> Self {
        let loaded = path
            .as_deref()
            .ok_or_else(storage_unavailable)
            .and_then(Self::read);
        let (choice, persistence) = match loaded {
            Ok(choice) => (choice, Persistence::Saved),
            Err(error) => {
                tracing::warn!(
                    operation = "desktop.preference.load_failed",
                    session = std::process::id(), key = C::KEY, ?path, %error
                );
                (C::default(), Persistence::LoadFailed)
            }
        };
        tracing::info!(
            operation = "desktop.preference.loaded",
            session = std::process::id(),
            key = C::KEY,
            choice = choice.code(),
            ?path,
        );
        Self {
            choice,
            persistence,
            path,
        }
    }

    pub fn select(&mut self, choice: C) {
        let previous = self.choice;
        // A storage failure must not prevent a live preference change. The view
        // retains the failure so choosing the same value can retry the save.
        self.choice = choice;
        self.persistence = match self.save() {
            Ok(()) => Persistence::Saved,
            Err(error) => {
                tracing::warn!(
                    operation = "desktop.preference.save_failed",
                    session = std::process::id(), key = C::KEY,
                    choice = choice.code(), path = ?self.path, %error
                );
                Persistence::SaveFailed
            }
        };
        tracing::info!(
            operation = "desktop.preference.changed",
            session = std::process::id(),
            key = C::KEY,
            previous = previous.code(),
            choice = choice.code(),
            saved = self.persistence == Persistence::Saved,
        );
    }

    fn read(path: &Path) -> io::Result<C> {
        match fs::read_to_string(path) {
            Ok(value) => C::parse(value.trim()).ok_or_else(|| {
                io::Error::new(io::ErrorKind::InvalidData, "unknown desktop preference")
            }),
            Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(C::default()),
            Err(error) => Err(error),
        }
    }

    fn save(&self) -> io::Result<()> {
        let path = self.path.as_deref().ok_or_else(storage_unavailable)?;
        let parent = path.parent().ok_or_else(|| {
            io::Error::new(
                io::ErrorKind::InvalidInput,
                "preference file needs a parent directory",
            )
        })?;
        fs::create_dir_all(parent)?;
        // Replace atomically so interruption cannot truncate the previous choice.
        let mut pending = tempfile::NamedTempFile::new_in(parent)?;
        pending.write_all(self.choice.code().as_bytes())?;
        pending.as_file().sync_all()?;
        pending.persist(path).map_err(|error| error.error)?;
        Ok(())
    }
}

fn storage_unavailable() -> io::Error {
    io::Error::new(
        io::ErrorKind::NotFound,
        "user configuration directory unavailable",
    )
}

pub fn init(cx: &mut gpui::App) {
    cx.set_global(crate::localization::LanguageSettings::load());
    let appearance = AppearanceSettings::load();
    cx.set_global(appearance.choice);
    cx.set_global(appearance);
}

#[cfg(test)]
#[path = "preferences_tests.rs"]
mod tests;
