use super::*;
use crate::localization::Language;

#[test]
fn rejected_email_code_asks_for_a_new_email_only_when_the_code_is_unusable() {
    let code = |purpose| AccountPage::Email(purpose, EmailStep::Code);
    // A mistyped registration code keeps its challenge, so the user only has to retype it.
    let retype = code(EmailPurpose::Registration).email_code_rejection(true);
    assert!(matches!(retype, Text::EmailCodeIncorrect));
    for language in [Language::English, Language::SimplifiedChinese] {
        assert_ne!(
            language.text(retype),
            language.text(Text::EmailCodeRejected)
        );
    }
    for (page, email_code_pending) in [
        // Expired, used up or replaced: the client no longer holds a code to retry.
        (code(EmailPurpose::Registration), false),
        (
            AccountPage::Email(EmailPurpose::Registration, EmailStep::Address),
            false,
        ),
        // Profile verification answers every failed code the same way, so it cannot promise a retry.
        (code(EmailPurpose::Profile), true),
    ] {
        assert!(
            matches!(
                page.email_code_rejection(email_code_pending),
                Text::EmailCodeRejected
            ),
            "{page:?}"
        );
    }
}
