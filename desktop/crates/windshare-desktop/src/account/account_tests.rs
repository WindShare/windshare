use super::*;
use windshare_host::{AccountProfile, AccountStatus, Allowance};

fn account_view(account: &str, state: AccountState) -> AccountView {
    AccountView {
        status: AccountStatus {
            active: true,
            state,
            email_code_pending: false,
            account: account.into(),
            credential: String::new(),
            username: String::new(),
            profile: AccountProfile::default(),
            pending_email: None,
            allowance: Allowance::Stale,
            remaining_bytes: 0,
            reset_at: String::new(),
            reason: String::new(),
        },
        reset_local: String::new(),
        nickname_change_local: String::new(),
        operation: None,
    }
}

fn model_with(account: &str, state: AccountState, request: AccountRequest) -> Model {
    Model {
        account: Some(account_view(account, state)),
        account_request: request,
        ..Model::default()
    }
}

fn failure(code: &str, message: &str) -> Failure {
    Failure::new(code, message)
}

#[test]
fn dismissed_registration_surfaces_failure_once_on_account_settings() {
    let pending = model_with(
        "",
        AccountState::Registering {
            registration_permille: 300,
        },
        AccountRequest::Pending {
            id: "account:1".into(),
            action: AccountAction::Register,
        },
    );
    let failed = model_with(
        "",
        AccountState::RegistrationRequired,
        AccountRequest::Failed {
            id: "account:1".into(),
            error: failure("account_service_unreachable", "unreachable"),
        },
    );

    // Dismissing on Share page retains the background registration across General settings.
    let mut state = AccountSettingsState::default();
    assert_eq!(
        state.observe_overlay_dismissed(&pending, false),
        Some("account:1")
    );
    assert_eq!(
        state,
        AccountSettingsState::BackgroundRegistration("account:1".into())
    );
    assert!(state.inline_feedback(&failed).is_none());

    // Passing through General settings after failure must not drop the unseen failure.
    assert_eq!(state.leave_settings(&failed), None);
    assert_eq!(
        state,
        AccountSettingsState::BackgroundRegistration("account:1".into())
    );

    // Entering Official Account settings surfaces the failure once.
    assert_eq!(state.enter_settings(&failed), Some("account:1"));
    assert!(matches!(
        state.inline_feedback(&failed),
        Some(Err(AccountFormError::Command(ref err))) if err.code == "account_service_unreachable"
    ));

    // Leaving after viewing clears the settled error so it does not resurface.
    assert_eq!(state.leave_settings(&failed), None);
    assert_eq!(state, AccountSettingsState::Idle);
    assert_eq!(state.enter_settings(&failed), None);
    assert!(state.inline_feedback(&failed).is_none());
}

#[test]
fn inline_registration_survives_navigation_round_trip_while_running() {
    let pending = model_with(
        "",
        AccountState::Registering {
            registration_permille: 640,
        },
        AccountRequest::Pending {
            id: "account:2".into(),
            action: AccountAction::Register,
        },
    );
    let failed = model_with(
        "",
        AccountState::RegistrationRequired,
        AccountRequest::Failed {
            id: "account:2".into(),
            error: failure("rate_limited", "slow down"),
        },
    );

    let mut state = AccountSettingsState::default();
    state.track_registration(Some("account:2".into()));
    assert_eq!(state.leave_settings(&pending), Some("account:2"));
    assert_eq!(
        state,
        AccountSettingsState::BackgroundRegistration("account:2".into())
    );

    assert_eq!(state.enter_settings(&pending), Some("account:2"));
    assert_eq!(
        state,
        AccountSettingsState::Registration("account:2".into())
    );
    assert!(state.inline_feedback(&pending).is_none());
    assert!(matches!(
        state.inline_feedback(&failed),
        Some(Err(AccountFormError::Command(ref err))) if err.code == "rate_limited"
    ));
}

#[test]
fn non_registration_modal_requests_never_pollute_account_settings() {
    for action in [
        AccountAction::Login,
        AccountAction::Password,
        AccountAction::Nickname,
    ] {
        let pending = model_with(
            "",
            AccountState::RegistrationRequired,
            AccountRequest::Pending {
                id: "account:3".into(),
                action,
            },
        );
        let failed = model_with(
            "",
            AccountState::RegistrationRequired,
            AccountRequest::Failed {
                id: "account:3".into(),
                error: failure("username_taken", "taken"),
            },
        );

        let mut state = AccountSettingsState::default();
        assert_eq!(state.observe_overlay_dismissed(&pending, true), None);
        assert_eq!(state.observe_overlay_dismissed(&pending, false), None);
        assert_eq!(state.enter_settings(&pending), None);
        assert_eq!(state, AccountSettingsState::Idle);
        assert!(state.inline_feedback(&failed).is_none());
    }
}

#[test]
fn settled_dialog_failure_does_not_resurface_and_preserves_other_overlays() {
    let settled_registration = model_with(
        "",
        AccountState::RegistrationRequired,
        AccountRequest::Failed {
            id: "account:4".into(),
            error: failure("rate_limited", "already shown in dialog"),
        },
    );
    let mut state = AccountSettingsState::default();
    assert_eq!(
        state.observe_overlay_dismissed(&settled_registration, true),
        None
    );
    assert_eq!(state.enter_settings(&settled_registration), None);
    assert!(state.inline_feedback(&settled_registration).is_none());

    // Opening and closing an unrelated overlay (e.g. About) keeps active refresh feedback.
    let failed_refresh = model_with(
        "acct-1",
        AccountState::TemporarilyUnavailable,
        AccountRequest::Failed {
            id: "account:5".into(),
            error: failure("account_service_unreachable", "offline"),
        },
    );
    state.track_refresh(Some("account:5".into()));
    assert_eq!(state.observe_overlay_dismissed(&failed_refresh, true), None);
    assert!(matches!(
        state.inline_feedback(&failed_refresh),
        Some(Err(AccountFormError::Command(ref err))) if err.code == "account_service_unreachable"
    ));
}

#[test]
fn feedback_respects_onboarding_versus_active_account_scope() {
    let failed_registration_after_external_login = model_with(
        "acct-cli",
        AccountState::Ready,
        AccountRequest::Failed {
            id: "account:6".into(),
            error: failure("account_service_unreachable", "old registration error"),
        },
    );
    let mut state = AccountSettingsState::BackgroundRegistration("account:6".into());
    assert_eq!(
        state.enter_settings(&failed_registration_after_external_login),
        None
    );
    assert_eq!(state, AccountSettingsState::Idle);

    // Even if already viewing onboarding when an external login lands, registration errors hide.
    let active_registration = AccountSettingsState::Registration("account:6".into());
    assert!(
        active_registration
            .inline_feedback(&failed_registration_after_external_login)
            .is_none()
    );

    // Refresh errors never surface on the onboarding page.
    let refresh_without_account = model_with(
        "",
        AccountState::RegistrationRequired,
        AccountRequest::Failed {
            id: "account:7".into(),
            error: failure("account_service_unreachable", "offline"),
        },
    );
    let refresh = AccountSettingsState::Refresh("account:7".into());
    assert!(refresh.inline_feedback(&refresh_without_account).is_none());
}

#[test]
fn dialog_page_only_reattaches_to_its_own_pending_action() {
    assert!(AccountPage::Registration.matches_pending_action(AccountAction::Register));
    assert!(!AccountPage::Registration.matches_pending_action(AccountAction::Login));
    assert!(AccountPage::Login.matches_pending_action(AccountAction::Login));
    assert!(!AccountPage::Login.matches_pending_action(AccountAction::Password));
    assert!(AccountPage::Credentials(()).matches_pending_action(AccountAction::Password));
    assert!(AccountPage::Nickname(()).matches_pending_action(AccountAction::Nickname));
    assert!(
        AccountPage::Email(EmailPurpose::Registration, EmailStep::Address)
            .matches_pending_action(AccountAction::Email)
    );
    assert!(
        AccountPage::Email(EmailPurpose::Registration, EmailStep::Code)
            .matches_pending_action(AccountAction::Verify)
    );
    assert!(
        AccountPage::Email(EmailPurpose::Profile(()), EmailStep::Address)
            .matches_pending_action(AccountAction::RequestProfileEmail)
    );
    assert!(
        AccountPage::Email(EmailPurpose::Profile(()), EmailStep::Code)
            .matches_pending_action(AccountAction::ConfirmProfileEmail)
    );
    assert!(!AccountPage::Overview.matches_pending_action(AccountAction::Register));
}
