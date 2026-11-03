use super::Language;
use std::{
    fs,
    io::{self, Write},
    path::PathBuf,
};

const LANGUAGE_FILE: &str = "language";

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum LanguageChoice {
    System,
    Language(Language),
}

impl LanguageChoice {
    pub const ALL: [Self; 3] = [
        Self::System,
        Self::Language(Language::SimplifiedChinese),
        Self::Language(Language::English),
    ];

    pub const fn code(self) -> &'static str {
        match self {
            Self::System => "system",
            Self::Language(language) => language.code(),
        }
    }

    fn parse(value: &str) -> io::Result<Self> {
        match value.trim() {
            "system" => Ok(Self::System),
            "en" => Ok(Self::Language(Language::English)),
            "zh-CN" => Ok(Self::Language(Language::SimplifiedChinese)),
            _ => Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "unknown desktop language",
            )),
        }
    }

    pub const fn resolve(self, system: Language) -> Language {
        match self {
            Self::System => system,
            Self::Language(language) => language,
        }
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
pub enum Persistence {
    Saved,
    LoadFailed,
    SaveFailed,
}

pub struct LanguageSettings {
    pub choice: LanguageChoice,
    pub persistence: Persistence,
    system: Language,
    store: Option<LanguageStore>,
}

impl LanguageSettings {
    pub fn load() -> Self {
        let system_locale = sys_locale::get_locale();
        let system = Language::from_system_locale(system_locale.as_deref());
        let store = dirs::config_dir().map(|directory| LanguageStore {
            path: directory
                .join("WindShare")
                .join("desktop")
                .join(LANGUAGE_FILE),
        });
        let loaded = store.as_ref().map_or_else(
            || {
                Err(io::Error::new(
                    io::ErrorKind::NotFound,
                    "user configuration directory unavailable",
                ))
            },
            LanguageStore::load,
        );
        let (choice, persistence) = match loaded {
            Ok(choice) => (choice, Persistence::Saved),
            Err(error) => {
                tracing::warn!(operation = "desktop.language.load_failed", session = std::process::id(), %error);
                (LanguageChoice::System, Persistence::LoadFailed)
            }
        };
        tracing::info!(
            operation = "desktop.language.loaded", session = std::process::id(),
            choice = choice.code(), language = choice.resolve(system).code(),
            ?system_locale, path = ?store.as_ref().map(|store| &store.path)
        );
        Self {
            choice,
            persistence,
            system,
            store,
        }
    }

    pub const fn language(&self) -> Language {
        self.choice.resolve(self.system)
    }

    pub fn select(&mut self, choice: LanguageChoice) {
        let previous = self.choice;
        self.choice = choice;
        // Apply the choice even when storage fails; a persistent inline notice offers a retry.
        let saved = self.store.as_ref().map_or_else(
            || {
                Err(io::Error::new(
                    io::ErrorKind::NotFound,
                    "user configuration directory unavailable",
                ))
            },
            |store| store.save(choice),
        );
        self.persistence = match saved {
            Ok(()) => Persistence::Saved,
            Err(error) => {
                tracing::warn!(operation = "desktop.language.save_failed", session = std::process::id(), choice = choice.code(), %error);
                Persistence::SaveFailed
            }
        };
        tracing::info!(
            operation = "desktop.language.changed",
            session = std::process::id(),
            previous = previous.code(),
            choice = choice.code(),
            language = self.language().code(),
            saved = self.persistence == Persistence::Saved
        );
    }
}

struct LanguageStore {
    path: PathBuf,
}

impl LanguageStore {
    fn load(&self) -> io::Result<LanguageChoice> {
        match fs::read_to_string(&self.path) {
            Ok(value) => LanguageChoice::parse(&value),
            Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(LanguageChoice::System),
            Err(error) => Err(error),
        }
    }

    fn save(&self, choice: LanguageChoice) -> io::Result<()> {
        let parent = self.path.parent().ok_or_else(|| {
            io::Error::new(
                io::ErrorKind::InvalidInput,
                "language file needs a parent directory",
            )
        })?;
        fs::create_dir_all(parent)?;
        // Replace atomically so an interrupted write cannot truncate the last saved choice.
        let mut pending = tempfile::NamedTempFile::new_in(parent)?;
        pending.write_all(choice.code().as_bytes())?;
        pending.as_file().sync_all()?;
        pending.persist(&self.path).map_err(|error| error.error)?;
        Ok(())
    }
}

#[cfg(test)]
#[path = "preferences_tests.rs"]
mod tests;
