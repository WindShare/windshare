use super::*;
use crate::localization::Language;
use windshare_host::{AccountProfile, AccountStatus, Allowance};

fn view(email_code_pending: bool) -> AccountView {
    AccountView {
        status: AccountStatus {
            active: true,
            state: AccountState::EmailRequired,
            email_code_pending,
            account: String::new(),
            credential: String::new(),
            username: String::new(),
            profile: AccountProfile::default(),
            pending_email: None,
            allowance: Allowance::Available,
            remaining_bytes: 0,
            reset_at: String::new(),
            reason: String::new(),
        },
        reset_local: String::new(),
        nickname_change_local: String::new(),
        operation: None,
    }
}

#[test]
fn rejected_email_code_asks_for_a_new_email_only_when_the_code_is_unusable() {
    let code = |purpose| AccountPage::Email(purpose, EmailStep::Code);
    // A mistyped registration code keeps its challenge, so the user only has to retype it.
    let retype = code(EmailPurpose::Registration).email_code_rejection(&view(true));
    assert!(matches!(retype, Text::EmailCodeIncorrect));
    for language in [Language::English, Language::SimplifiedChinese] {
        assert_ne!(
            language.text(retype),
            language.text(Text::EmailCodeRejected)
        );
    }
    for (page, view) in [
        // Expired, used up or replaced: the client no longer holds a code to retry.
        (code(EmailPurpose::Registration), view(false)),
        (
            AccountPage::Email(EmailPurpose::Registration, EmailStep::Address),
            view(false),
        ),
        // Profile verification answers every failed code the same way, so it cannot promise a retry.
        (code(EmailPurpose::Profile), view(true)),
    ] {
        assert!(
            matches!(page.email_code_rejection(&view), Text::EmailCodeRejected),
            "{page:?}"
        );
    }
}
