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
use windshare_host::{AccountRequest, AccountState, AccountView, Failure, Model};

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
}

#[derive(Clone, Debug)]
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

#[derive(Default)]
pub struct AccountSettingsState {
    pub inline_request_id: Option<String>,
}

impl AccountSettingsState {
    pub fn clear(&mut self) {
        self.inline_request_id = None;
    }

    pub fn inline_feedback(&self, model: &Model) -> Option<Result<Text, AccountFormError>> {
        let tracked = self.inline_request_id.as_deref()?;
        match &model.account_request {
            AccountRequest::Failed { id, error } if id == tracked => {
                Some(Err(AccountFormError::from(error.clone())))
            }
            _ => None,
        }
    }
}
