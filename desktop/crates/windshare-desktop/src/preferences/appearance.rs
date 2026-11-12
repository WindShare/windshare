use super::{Preference, PreferenceChoice};
use gpui::{App, BorrowAppContext, Global};
use windshare_ui::{Theme, ThemePreference};

pub type AppearanceSettings = Preference<ThemePreference>;

impl Global for AppearanceSettings {}

impl PreferenceChoice for ThemePreference {
    const KEY: &'static str = "appearance";

    fn code(self) -> &'static str {
        match self {
            Self::System => "system",
            Self::Explicit(Theme::Light) => "light",
            Self::Explicit(Theme::Dark) => "dark",
        }
    }

    fn parse(value: &str) -> Option<Self> {
        match value {
            "system" => Some(Self::System),
            "light" => Some(Self::Explicit(Theme::Light)),
            "dark" => Some(Self::Explicit(Theme::Dark)),
            _ => None,
        }
    }
}

pub fn select_appearance(choice: ThemePreference, cx: &mut App) {
    cx.update_global::<AppearanceSettings, _>(|settings, _| settings.select(choice));
    // The rendering crate receives only the choice; storage stays application-owned.
    cx.set_global(choice);
}
