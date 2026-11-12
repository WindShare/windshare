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
