use super::*;
use crate::localization::LanguageChoice;
use gpui::WindowAppearance;
use windshare_ui::{Theme, ThemePreference};

#[test]
fn appearance_follows_system_only_without_an_explicit_choice() {
    for (appearance, expected) in [
        (WindowAppearance::Light, Theme::Light),
        (WindowAppearance::VibrantLight, Theme::Light),
        (WindowAppearance::Dark, Theme::Dark),
        (WindowAppearance::VibrantDark, Theme::Dark),
    ] {
        assert_eq!(ThemePreference::System.resolve(appearance), expected);
        for theme in [Theme::Light, Theme::Dark] {
            assert_eq!(ThemePreference::Explicit(theme).resolve(appearance), theme);
        }
    }
}

fn assert_round_trip<C: PreferenceChoice + PartialEq + std::fmt::Debug>(choices: &[C]) {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("desktop").join(C::KEY);
    let mut settings = Preference::<C>::load_from(Some(path.clone()));
    assert_eq!(settings.choice, C::default());
    assert_eq!(settings.persistence, Persistence::Saved);
    assert!(!path.exists());
    for &choice in choices {
        settings.select(choice);
        assert_eq!(settings.persistence, Persistence::Saved);
        let reopened = Preference::<C>::load_from(Some(path.clone()));
        assert_eq!(reopened.choice, choice);
        assert_eq!(reopened.persistence, Persistence::Saved);
    }
    assert_eq!(fs::read_dir(path.parent().unwrap()).unwrap().count(), 1);
}

#[test]
fn preferences_default_to_system_and_survive_restart() {
    assert_round_trip(&LanguageChoice::ALL);
    assert_round_trip(&ThemePreference::ALL);
}

#[test]
fn appearance_and_language_save_independently() {
    let directory = tempfile::tempdir().unwrap();
    let language_path = directory.path().join(LanguageChoice::KEY);
    let appearance_path = directory.path().join(ThemePreference::KEY);
    let mut language = Preference::<LanguageChoice>::load_from(Some(language_path.clone()));
    let mut appearance = AppearanceSettings::load_from(Some(appearance_path.clone()));
    language.select(LanguageChoice::Language(
        crate::localization::Language::English,
    ));
    appearance.select(ThemePreference::Explicit(Theme::Dark));
    assert_eq!(
        Preference::<LanguageChoice>::read(&language_path).unwrap(),
        language.choice
    );
    assert_eq!(
        AppearanceSettings::read(&appearance_path).unwrap(),
        appearance.choice
    );
}

#[test]
fn corrupt_preference_falls_back_and_can_be_replaced() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join(ThemePreference::KEY);
    for invalid in [b"unsupported-theme".as_slice(), b"", &[0xff]] {
        fs::write(&path, invalid).unwrap();
        let mut settings = AppearanceSettings::load_from(Some(path.clone()));
        assert_eq!(settings.choice, ThemePreference::System);
        assert_eq!(settings.persistence, Persistence::LoadFailed);
        let choice = ThemePreference::Explicit(Theme::Dark);
        settings.select(choice);
        assert_eq!(settings.persistence, Persistence::Saved);
        assert_eq!(AppearanceSettings::read(&path).unwrap(), choice);
    }
}

#[test]
fn failed_save_keeps_live_choice_and_can_be_retried() {
    let directory = tempfile::tempdir().unwrap();
    let parent = directory.path().join("blocked");
    fs::write(&parent, "not a directory").unwrap();
    let path = parent.join(ThemePreference::KEY);
    let mut settings = AppearanceSettings::load_from(Some(path.clone()));
    let choice = ThemePreference::Explicit(Theme::Dark);
    settings.select(choice);
    assert_eq!(settings.choice, choice);
    assert_eq!(settings.persistence, Persistence::SaveFailed);
    fs::remove_file(&parent).unwrap();
    settings.select(choice);
    assert_eq!(settings.persistence, Persistence::Saved);
    assert_eq!(AppearanceSettings::read(&path).unwrap(), choice);
}

#[test]
fn unavailable_storage_does_not_prevent_preference_changes() {
    let mut settings = AppearanceSettings::load_from(None);
    assert_eq!(settings.choice, ThemePreference::System);
    assert_eq!(settings.persistence, Persistence::LoadFailed);
    settings.select(ThemePreference::Explicit(Theme::Light));
    assert_eq!(settings.choice, ThemePreference::Explicit(Theme::Light));
    assert_eq!(settings.persistence, Persistence::SaveFailed);
}
