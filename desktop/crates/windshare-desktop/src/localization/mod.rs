mod catalog;
mod formatting;
mod preferences;
mod view;

pub use catalog::Text;
pub use preferences::{LanguageChoice, LanguageSettings};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Language {
    English,
    SimplifiedChinese,
}

impl Language {
    pub fn from_system_locale(locale: Option<&str>) -> Self {
        let language = locale
            .unwrap_or_default()
            .split(['-', '_', '.', '@'])
            .next();
        if language.is_some_and(|language| language.eq_ignore_ascii_case("zh")) {
            Self::SimplifiedChinese
        } else {
            Self::English
        }
    }

    pub const fn code(self) -> &'static str {
        match self {
            Self::English => "en",
            Self::SimplifiedChinese => "zh-CN",
        }
    }

    pub const fn native_name(self) -> &'static str {
        match self {
            Self::English => "English",
            Self::SimplifiedChinese => "简体中文",
        }
    }
}

impl gpui::Global for LanguageSettings {}

pub fn language(cx: &gpui::App) -> Language {
    cx.global::<LanguageSettings>().language()
}
