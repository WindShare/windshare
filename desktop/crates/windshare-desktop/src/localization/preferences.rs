use super::Language;
use crate::preferences::{Preference, PreferenceChoice};

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum LanguageChoice {
    #[default]
    System,
    Language(Language),
}

impl LanguageChoice {
    pub const ALL: [Self; 3] = [
        Self::System,
        Self::Language(Language::SimplifiedChinese),
        Self::Language(Language::English),
    ];

    pub const fn resolve(self, system: Language) -> Language {
        match self {
            Self::System => system,
            Self::Language(language) => language,
        }
    }
}

impl PreferenceChoice for LanguageChoice {
    const KEY: &'static str = "language";

    fn code(self) -> &'static str {
        match self {
            Self::System => "system",
            Self::Language(language) => language.code(),
        }
    }

    fn parse(value: &str) -> Option<Self> {
        match value {
            "system" => Some(Self::System),
            "en" => Some(Self::Language(Language::English)),
            "zh-CN" => Some(Self::Language(Language::SimplifiedChinese)),
            _ => None,
        }
    }
}

pub struct LanguageSettings {
    pub preference: Preference<LanguageChoice>,
    system: Language,
}

impl LanguageSettings {
    pub fn load() -> Self {
        let system_locale = sys_locale::get_locale();
        let system = Language::from_system_locale(system_locale.as_deref());
        let preference = Preference::load();
        let settings = Self { preference, system };
        tracing::info!(
            operation = "desktop.language.resolved",
            session = std::process::id(),
            language = settings.language().code(),
            ?system_locale,
        );
        settings
    }

    pub const fn language(&self) -> Language {
        self.preference.choice.resolve(self.system)
    }

    pub fn select(&mut self, choice: LanguageChoice) {
        self.preference.select(choice);
    }
}

#[cfg(test)]
#[path = "preferences_tests.rs"]
mod tests;
