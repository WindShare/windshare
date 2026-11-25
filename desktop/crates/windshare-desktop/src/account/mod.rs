mod dialog;
mod dialog_view;
mod host;
mod notice;
mod registration;
mod settings;

pub use dialog::{
    AccountDialog, AccountDialogEvent, AccountOrigin, AccountSource, PreviewAccountEdit,
};
pub use notice::account_status_summary;

use crate::localization::Text;
use windshare_host::{AccountAction, AccountRequest, AccountState, AccountView, Failure, Model};

#[cfg(test)]
#[path = "account_tests.rs"]
mod tests;

// Navigation selects a page. An open editor also owns the account it was opened for.
pub type AccountPage = AccountScreen<()>;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AccountScreen<Target> {
    Overview,
    Registration,
    Login,
    Credentials(Target),
    Nickname(Target),
    Email(EmailPurpose<Target>, EmailStep),
}

impl<Target> AccountScreen<Target> {
    pub(crate) fn map_target<T>(&self, map: impl FnOnce(&Target) -> T) -> AccountScreen<T> {
        match self {
            Self::Overview => AccountScreen::Overview,
            Self::Registration => AccountScreen::Registration,
            Self::Login => AccountScreen::Login,
            Self::Credentials(target) => AccountScreen::Credentials(map(target)),
            Self::Nickname(target) => AccountScreen::Nickname(map(target)),
            Self::Email(EmailPurpose::Registration, step) => {
                AccountScreen::Email(EmailPurpose::Registration, *step)
            }
            Self::Email(EmailPurpose::Profile(target), step) => {
                AccountScreen::Email(EmailPurpose::Profile(map(target)), *step)
            }
        }
    }

    pub(crate) const fn target(&self) -> Option<&Target> {
        match self {
            Self::Credentials(target)
            | Self::Nickname(target)
            | Self::Email(EmailPurpose::Profile(target), _) => Some(target),
            _ => None,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum EmailPurpose<Target = ()> {
    Registration,
    Profile(Target),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum EmailStep {
    Address,
    Code,
}

impl<Target> EmailPurpose<Target> {
    pub(crate) const fn has_code(&self, view: &AccountView) -> bool {
        match self {
            Self::Registration => view.status.email_code_pending,
            Self::Profile(_) => view.status.pending_email.is_some(),
        }
    }

    pub(crate) const fn step(&self, view: &AccountView) -> EmailStep {
        if self.has_code(view) {
            EmailStep::Code
        } else {
            EmailStep::Address
        }
    }

    pub(crate) const fn entry(self, view: &AccountView) -> AccountScreen<Target> {
        let step = self.step(view);
        AccountScreen::Email(self, step)
    }
}

impl<Target> AccountScreen<Target> {
    pub(crate) const fn entry(view: &AccountView) -> (Self, Text) {
        match view.status.state {
            AccountState::EmailRequired => {
                (EmailPurpose::Registration.entry(view), Text::AccountEmail)
            }
            AccountState::Registering { .. } => {
                (Self::Registration, Text::ViewRegistrationProgress)
            }
            AccountState::PendingSetup
            | AccountState::RegistrationRequired
            | AccountState::TemporarilyUnavailable
                if view.status.account.is_empty() =>
            {
                (Self::Registration, Text::SetUpAccount)
            }
            _ => (Self::Overview, Text::ViewAccount),
        }
    }
}

impl AccountPage {
    pub(crate) const fn title(self, view: &AccountView) -> Text {
        match self {
            Self::Overview | Self::Registration => Text::Account,
            Self::Login => Text::LoginExisting,
            Self::Credentials(()) if view.status.username.is_empty() => Text::SetAccountLogin,
            Self::Credentials(()) => Text::EditAccountLogin,
            Self::Nickname(()) if view.status.profile.nickname.is_none() => Text::SetNickname,
            Self::Nickname(()) => Text::EditNickname,
            Self::Email(EmailPurpose::Profile(()), EmailStep::Address)
                if view.status.profile.email.is_empty() =>
            {
                Text::SetProfileEmail
            }
            Self::Email(EmailPurpose::Profile(()), EmailStep::Address) => Text::EditProfileEmail,
            Self::Email(_, _) => Text::AccountEmail,
        }
    }

    pub(crate) const fn matches_pending_action(self, action: AccountAction) -> bool {
        matches!(
            (self, action),
            (Self::Registration, AccountAction::Register)
                | (Self::Login, AccountAction::Login)
                | (Self::Credentials(()), AccountAction::Password)
                | (Self::Nickname(()), AccountAction::Nickname)
                | (
                    Self::Email(EmailPurpose::Registration, EmailStep::Address),
                    AccountAction::Email
                )
                | (
                    Self::Email(EmailPurpose::Registration, EmailStep::Code),
                    AccountAction::Verify
                )
                | (
                    Self::Email(EmailPurpose::Profile(()), EmailStep::Address),
                    AccountAction::RequestProfileEmail
                )
                | (
                    Self::Email(EmailPurpose::Profile(()), EmailStep::Code),
                    AccountAction::ConfirmProfileEmail
                )
        )
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum AccountFormError {
    AccountChanged,
    Command(Failure),
}

impl From<Failure> for AccountFormError {
    fn from(error: Failure) -> Self {
        if error.code == "account_changed" {
            Self::AccountChanged
        } else {
            Self::Command(error)
        }
    }
}

pub const fn can_register_account(view: &AccountView, busy: bool) -> bool {
    !busy
        && view.status.account.is_empty()
        && !matches!(
            view.status.state,
            AccountState::Registering { .. } | AccountState::Denied | AccountState::StorageError
        )
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RegistrationFailureHandoff {
    pub request_id: String,
    pub error: AccountFormError,
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub enum AccountSettingsState {
    #[default]
    Idle,
    /// Tracks an active or settled registration on the onboarding settings page.
    Registration(String),
    /// Retains an in-flight registration after the user dismissed its modal or
    /// navigated away from Official Account settings so an unseen failure
    /// surfaces once upon returning to check progress.
    BackgroundRegistration(String),
    /// Tracks an inline refresh started from the active account settings page.
    Refresh(String),
}

impl AccountSettingsState {
    pub fn clear(&mut self) {
        *self = Self::Idle;
    }

    pub fn track_registration(&mut self, request_id: Option<String>) {
        *self = request_id.map_or(Self::Idle, Self::Registration);
    }

    pub fn track_refresh(&mut self, request_id: Option<String>) {
        *self = request_id.map_or(Self::Idle, Self::Refresh);
    }

    pub const fn tracked_id(&self) -> Option<&str> {
        match self {
            Self::Idle => None,
            Self::Registration(id) | Self::BackgroundRegistration(id) | Self::Refresh(id) => {
                Some(id.as_str())
            }
        }
    }

    fn unseen_registration_failure(id: &str, model: &Model) -> Option<AccountFormError> {
        let onboarding = model
            .account
            .as_ref()
            .is_some_and(|view| view.status.account.is_empty());
        if !onboarding {
            return None;
        }
        match &model.account_request {
            AccountRequest::Failed {
                id: failed_id,
                error,
            } if failed_id == id => Some(AccountFormError::from(error.clone())),
            _ => None,
        }
    }

    /// Reconciles settings/background state before opening an account modal.
    ///
    /// Opening `AccountPage::Registration` shows the same registration surface
    /// as Settings, so an unseen background failure is consumed once and handed
    /// off to the dialog instead of being discarded or deferred to Settings.
    pub fn prepare_modal(
        &mut self,
        page: AccountPage,
        model: &Model,
    ) -> Option<RegistrationFailureHandoff> {
        if page == AccountPage::Registration {
            return match std::mem::take(self) {
                Self::BackgroundRegistration(request_id) => {
                    Self::unseen_registration_failure(&request_id, model)
                        .map(|error| RegistrationFailureHandoff { request_id, error })
                }
                _ => None,
            };
        }
        let retain = matches!(
            self,
            Self::BackgroundRegistration(id)
                if model.in_flight_registration_id() == Some(id.as_str())
                    || Self::unseen_registration_failure(id, model).is_some()
        );
        if !retain {
            *self = Self::Idle;
        }
        None
    }

    pub fn observe_overlay_dismissed(
        &mut self,
        model: &Model,
        settings_visible: bool,
    ) -> Option<&str> {
        if let Some(id) = model.in_flight_registration_id() {
            let next = if settings_visible {
                Self::Registration(id.to_owned())
            } else {
                Self::BackgroundRegistration(id.to_owned())
            };
            if *self != next {
                *self = next;
                return self.tracked_id();
            }
            return None;
        }
        if settings_visible && matches!(self, Self::BackgroundRegistration(_)) {
            return self.enter_settings(model);
        }
        None
    }

    pub fn enter_settings(&mut self, model: &Model) -> Option<&str> {
        if let Some(id) = model.in_flight_registration_id() {
            let next = Self::Registration(id.to_owned());
            if *self != next {
                *self = next;
                return self.tracked_id();
            }
            return None;
        }
        if let Self::BackgroundRegistration(id) = std::mem::take(self)
            && Self::unseen_registration_failure(&id, model).is_some()
        {
            *self = Self::Registration(id);
            return self.tracked_id();
        }
        *self = Self::Idle;
        None
    }

    pub fn leave_settings(&mut self, model: &Model) -> Option<&str> {
        if let Some(id) = model.in_flight_registration_id() {
            let next = Self::BackgroundRegistration(id.to_owned());
            if *self != next {
                *self = next;
                return self.tracked_id();
            }
            return None;
        }
        if !matches!(self, Self::BackgroundRegistration(_)) {
            *self = Self::Idle;
        }
        None
    }

    pub fn inline_feedback(&self, model: &Model) -> Option<Result<Text, AccountFormError>> {
        let view = model.account.as_ref()?;
        let tracked = match (self, view.status.account.is_empty()) {
            (Self::Registration(id), true) | (Self::Refresh(id), false) => id.as_str(),
            _ => return None,
        };
        match &model.account_request {
            AccountRequest::Failed { id, error } if id == tracked => {
                Some(Err(AccountFormError::from(error.clone())))
            }
            _ => None,
        }
    }
}
