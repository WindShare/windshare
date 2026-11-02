use super::*;

#[test]
fn resolves_system_language_and_explicit_override() {
    for (locale, expected) in [
        (None, Language::English),
        (Some(""), Language::English),
        (Some("en-US"), Language::English),
        (Some("zh-CN"), Language::SimplifiedChinese),
        (Some("ZH_hans_SG.UTF-8"), Language::SimplifiedChinese),
        (Some("fr-FR"), Language::English),
        (Some("zhx"), Language::English),
    ] {
        let system = Language::from_system_locale(locale);
        assert_eq!(system, expected);
        assert_eq!(LanguageChoice::System.resolve(system), expected);
        for language in [Language::English, Language::SimplifiedChinese] {
            assert_eq!(LanguageChoice::Language(language).resolve(system), language);
        }
    }
}

#[test]
fn missing_preference_follows_system_and_choices_survive_restart() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("desktop").join(LANGUAGE_FILE);
    let store = LanguageStore { path: path.clone() };
    assert_eq!(store.load().unwrap(), LanguageChoice::System);
    assert!(!path.exists());
    for choice in LanguageChoice::ALL {
        store.save(choice).unwrap();
        let reopened = LanguageStore { path: path.clone() };
        assert_eq!(reopened.load().unwrap(), choice);
    }
    assert_eq!(fs::read_dir(path.parent().unwrap()).unwrap().count(), 1);
}

#[test]
fn invalid_preference_is_reported_and_can_be_replaced() {
    let directory = tempfile::tempdir().unwrap();
    let store = LanguageStore {
        path: directory.path().join(LANGUAGE_FILE),
    };
    fs::write(&store.path, "unsupported-language").unwrap();
    assert_eq!(store.load().unwrap_err().kind(), io::ErrorKind::InvalidData);
    store
        .save(LanguageChoice::Language(Language::English))
        .unwrap();
    assert_eq!(
        store.load().unwrap(),
        LanguageChoice::Language(Language::English)
    );
    fs::write(&store.path, [0xff]).unwrap();
    assert!(store.load().is_err());
}

#[test]
fn failed_save_keeps_live_choice_and_can_be_retried() {
    let directory = tempfile::tempdir().unwrap();
    let parent = directory.path().join("blocked");
    fs::write(&parent, "not a directory").unwrap();
    let mut settings = LanguageSettings {
        choice: LanguageChoice::System,
        persistence: Persistence::Saved,
        system: Language::English,
        store: Some(LanguageStore {
            path: parent.join(LANGUAGE_FILE),
        }),
    };
    let chinese = LanguageChoice::Language(Language::SimplifiedChinese);
    settings.select(chinese);
    assert_eq!(settings.language(), Language::SimplifiedChinese);
    assert!(settings.persistence == Persistence::SaveFailed);
    fs::remove_file(&parent).unwrap();
    settings.select(chinese);
    assert!(settings.persistence == Persistence::Saved);
    assert_eq!(settings.store.as_ref().unwrap().load().unwrap(), chinese);
}

#[test]
fn unavailable_storage_does_not_prevent_language_changes() {
    let mut settings = LanguageSettings {
        choice: LanguageChoice::System,
        persistence: Persistence::LoadFailed,
        system: Language::SimplifiedChinese,
        store: None,
    };
    settings.select(LanguageChoice::Language(Language::English));
    assert_eq!(settings.language(), Language::English);
    assert!(settings.persistence == Persistence::SaveFailed);
}
