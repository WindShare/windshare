use super::Runtime;
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::Overlay,
};
use gpui::{Entity, FocusHandle, Focusable, Subscription, Window, prelude::*};
use windshare_host::{AccountCommand, AccountOutcome, AccountState, AccountView, Failure, Model};
use windshare_ui::{Input, InputEvent};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AccountPage {
    Overview,
    Registration,
    Login,
    Credentials,
    Nickname,
    Email(EmailPurpose, EmailStep),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum EmailPurpose {
    Registration,
    Profile,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum EmailStep {
    Address,
    Code,
}

impl EmailPurpose {
    pub(super) const fn has_code(self, view: &AccountView) -> bool {
        match self {
            Self::Registration => view.status.email_code_pending,
            Self::Profile => view.status.pending_email.is_some(),
        }
    }

    pub(super) const fn entry(self, view: &AccountView) -> AccountPage {
        AccountPage::Email(
            self,
            if self.has_code(view) {
                EmailStep::Code
            } else {
                EmailStep::Address
            },
        )
    }
}

impl AccountPage {
    pub(super) const fn entry(view: &AccountView) -> (Self, Text) {
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

    /// Registration keeps a mistyped code's challenge (the code is still pending), so only
    /// an expired, used-up or replaced code needs a new email. Profile verification cannot
    /// tell these apart, so it keeps the general wording.
    pub(super) const fn email_code_rejection(self, email_code_pending: bool) -> Text {
        match self {
            Self::Email(EmailPurpose::Registration, EmailStep::Code) if email_code_pending => {
                Text::EmailCodeIncorrect
            }
            _ => Text::EmailCodeRejected,
        }
    }
}

#[derive(Clone, Copy, Debug)]
pub enum AccountAction {
    Open(AccountPage),
    Submit,
    Retry,
}

pub struct AccountForm {
    pub(super) nickname: Entity<Input>,
    pub(super) username: Entity<Input>,
    pub(super) password: Entity<Input>,
    pub(super) email: Entity<Input>,
    pub(super) code: Entity<Input>,
    pub(super) page: AccountPage,
    pending: Option<String>,
    pub(super) feedback: Option<Result<Text, Failure>>,
    _subscriptions: Vec<Subscription>,
}

impl AccountForm {
    pub fn new(cx: &mut Context<Desktop>) -> Self {
        let locale = language(cx);
        let nickname = cx.new(|cx| Input::new(locale.text(Text::Nickname), cx));
        let username = cx.new(|cx| Input::new(locale.text(Text::AccountUsername), cx));
        let password =
            cx.new(|cx| Input::new(locale.text(Text::AccountPasswordLabel), cx).password());
        let email = cx.new(|cx| Input::new(locale.text(Text::AccountEmailLabel), cx));
        let code = cx.new(|cx| Input::new(locale.text(Text::AccountCode), cx));
        let subscriptions = [&nickname, &username, &password, &email, &code]
            .into_iter()
            .map(|input| {
                cx.subscribe(input, |this, _, event, cx| {
                    if matches!(event, InputEvent::Submit)
                        && this
                            .overlays
                            .last()
                            .is_some_and(|frame| matches!(frame.content, Overlay::Account))
                    {
                        this.submit_account(cx);
                    }
                    cx.notify();
                })
            })
            .collect();
        Self {
            nickname,
            username,
            password,
            email,
            code,
            page: AccountPage::Overview,
            pending: None,
            feedback: None,
            _subscriptions: subscriptions,
        }
    }

    pub fn localize(&self, cx: &mut Context<Desktop>) {
        let locale = language(cx);
        for (input, text) in [
            (&self.nickname, Text::Nickname),
            (&self.username, Text::AccountUsername),
            (&self.password, Text::AccountPasswordLabel),
            (&self.email, Text::AccountEmailLabel),
            (&self.code, Text::AccountCode),
        ] {
            input.update(cx, |input, cx| input.set_labels(locale.text(text), "", cx));
        }
    }

    pub(super) fn field_focus(&self, model: &Model, cx: &Context<Desktop>) -> Option<FocusHandle> {
        model.account.as_ref()?;
        let input = match self.page {
            AccountPage::Login | AccountPage::Credentials => &self.username,
            AccountPage::Nickname => &self.nickname,
            AccountPage::Email(_, EmailStep::Code) => &self.code,
            AccountPage::Email(_, EmailStep::Address) => &self.email,
            AccountPage::Overview | AccountPage::Registration => return None,
        };
        Some(input.read(cx).focus_handle(cx))
    }

    pub(super) const fn busy(&self, view: &AccountView) -> bool {
        self.pending.is_some() || view.busy()
    }

    pub(super) fn can_submit(&self, view: &AccountView, cx: &Context<Desktop>) -> bool {
        if self.busy(view) {
            return false;
        }
        match self.page {
            AccountPage::Overview => false,
            AccountPage::Registration => {
                view.status.account.is_empty()
                    && !matches!(
                        view.status.state,
                        AccountState::Registering { .. } | AccountState::Denied
                    )
            }
            AccountPage::Credentials if view.status.account.is_empty() => false,
            AccountPage::Login | AccountPage::Credentials => {
                !self.username.read(cx).value().trim().is_empty()
                    && !self.password.read(cx).value().is_empty()
            }
            AccountPage::Nickname => {
                let value = self.nickname.read(cx).value().trim();
                view.can_edit_profile()
                    && view.nickname_locked_until().is_none()
                    && !value.is_empty()
                    && view
                        .status
                        .profile
                        .nickname
                        .as_ref()
                        .is_none_or(|current| current.value != value)
            }
            AccountPage::Email(purpose, step) => {
                let available = match purpose {
                    EmailPurpose::Registration => view.status.state == AccountState::EmailRequired,
                    EmailPurpose::Profile => view.can_edit_profile(),
                };
                available
                    && match step {
                        EmailStep::Code => {
                            purpose.has_code(view) && !self.code.read(cx).value().trim().is_empty()
                        }
                        EmailStep::Address => {
                            let address = self.email.read(cx).value().trim();
                            !address.is_empty()
                                && (purpose == EmailPurpose::Registration
                                    || !address.eq_ignore_ascii_case(&view.status.profile.email))
                        }
                    }
            }
        }
    }

    pub(super) fn reconcile(&mut self, previous: &Model, model: &Model, cx: &mut Context<Desktop>) {
        let Some(view) = &model.account else { return };
        let switched = previous
            .account
            .as_ref()
            .is_none_or(|old| old.status.account != view.status.account);
        if switched
            && matches!(
                self.page,
                AccountPage::Nickname | AccountPage::Email(EmailPurpose::Profile, _)
            )
        {
            self.page = AccountPage::Overview;
            self.feedback = None;
            self.pending = None;
        }
        if let Some(id) = &self.pending {
            if let Some(op) = view.operation.as_ref().filter(|op| &op.id == id) {
                match &op.outcome {
                    AccountOutcome::Running => return,
                    AccountOutcome::Failed { error } => {
                        self.feedback = Some(Err(error.clone()));
                    }
                    AccountOutcome::Succeeded => self.completed(view, cx),
                }
            } else if let Some(error) = model.settings_rejection(id) {
                self.feedback = Some(Err(error.clone()));
            } else {
                return;
            }
            self.pending = None;
        }
        // A CLI login or a change in admission policy can resolve the current step.
        self.page = match self.page {
            AccountPage::Registration if !view.status.account.is_empty() => AccountPage::Overview,
            AccountPage::Registration if view.status.state == AccountState::EmailRequired => {
                EmailPurpose::Registration.entry(view)
            }
            AccountPage::Email(EmailPurpose::Registration, _)
                if view.status.state != AccountState::EmailRequired =>
            {
                AccountPage::entry(view).0
            }
            AccountPage::Nickname | AccountPage::Email(EmailPurpose::Profile, _)
                if !view.can_edit_profile() =>
            {
                AccountPage::Overview
            }
            AccountPage::Nickname if view.nickname_locked_until().is_some() => {
                AccountPage::Overview
            }
            AccountPage::Email(purpose, EmailStep::Code) if !purpose.has_code(view) => {
                AccountPage::Email(purpose, EmailStep::Address)
            }
            page => page,
        };
    }

    fn completed(&mut self, view: &AccountView, cx: &mut Context<Desktop>) {
        let message = match self.page {
            AccountPage::Registration if view.status.state == AccountState::EmailRequired => {
                self.page = EmailPurpose::Registration.entry(view);
                return;
            }
            AccountPage::Registration => Text::AccountCreated,
            AccountPage::Login => Text::AccountSignedIn,
            AccountPage::Credentials => Text::AccountLoginSaved,
            AccountPage::Nickname => Text::NicknameSaved,
            AccountPage::Email(purpose, EmailStep::Address) => {
                self.page = purpose.entry(view);
                self.code.update(cx, |input, cx| input.set_value("", cx));
                return;
            }
            AccountPage::Email(EmailPurpose::Registration, EmailStep::Code) => {
                Text::AccountVerified
            }
            AccountPage::Email(EmailPurpose::Profile, EmailStep::Code) => Text::ProfileEmailSaved,
            AccountPage::Overview => return,
        };
        self.password
            .update(cx, |input, cx| input.set_value("", cx));
        self.code.update(cx, |input, cx| input.set_value("", cx));
        self.page = AccountPage::Overview;
        self.feedback = Some(Ok(message));
    }
}

impl Desktop {
    pub(super) fn account_view(&self) -> Option<&AccountView> {
        if let Runtime::Native(session) = &self.runtime {
            session.model.account.as_ref()
        } else {
            None
        }
    }

    pub fn open_account(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(view) = self.account_view() else {
            return;
        };
        let page = AccountPage::entry(view).0;
        self.open_overlay(Overlay::Account, window, cx);
        if self.account_form.pending.is_none()
            && !(page == AccountPage::Registration && self.account_form.page == page)
        {
            self.set_account_page(page, window, cx);
        }
    }

    fn set_account_page(&mut self, page: AccountPage, window: &mut Window, cx: &mut Context<Self>) {
        let Some(view) = self.account_view() else {
            return;
        };
        let username = if page == AccountPage::Credentials {
            view.status.username.clone()
        } else {
            String::new()
        };
        let nickname = view
            .status
            .profile
            .nickname
            .as_ref()
            .map_or_default(|nickname| nickname.value.clone());
        let email = match page {
            AccountPage::Email(EmailPurpose::Profile, EmailStep::Address) => Some(
                view.status
                    .pending_email
                    .as_ref()
                    .map_or(&view.status.profile.email, |pending| &pending.address)
                    .clone(),
            ),
            AccountPage::Email(EmailPurpose::Registration, EmailStep::Address)
                if !matches!(
                    self.account_form.page,
                    AccountPage::Email(EmailPurpose::Registration, _)
                ) =>
            {
                Some(String::new())
            }
            _ => None,
        };
        let form = &mut self.account_form;
        form.page = page;
        if page == AccountPage::Nickname {
            form.nickname
                .update(cx, |input, cx| input.set_value(nickname, cx));
        }
        if let Some(email) = email {
            form.email
                .update(cx, |input, cx| input.set_value(email, cx));
        }
        form.feedback = None;
        form.password
            .update(cx, |input, cx| input.set_value("", cx));
        form.username
            .update(cx, |input, cx| input.set_value(username, cx));
        form.code.update(cx, |input, cx| input.set_value("", cx));
        if let Runtime::Native(session) = &self.runtime
            && let Some(focus) = form
                .field_focus(&session.model, cx)
                .or_else(|| self.overlays.last().map(|frame| frame.scope.clone()))
        {
            window.focus(&focus, cx);
        }
    }

    pub fn account_action(
        &mut self,
        action: AccountAction,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Some(view) = self.account_view() else {
            return;
        };
        if self.account_form.busy(view) {
            return;
        }
        match action {
            AccountAction::Open(page) => self.set_account_page(page, window, cx),
            AccountAction::Submit => {
                self.submit_account(cx);
                // The primary button leaves the tree while the account is being created.
                if self.account_form.page == AccountPage::Registration
                    && self.account_form.pending.is_some()
                    && let Some(frame) = self.overlays.last()
                {
                    window.focus(&frame.scope, cx);
                }
            }
            AccountAction::Retry => self.send_account_command(AccountCommand::Retry),
        }
        cx.notify();
    }

    fn submit_account(&mut self, cx: &Context<Self>) {
        let Some(view) = self.account_view() else {
            return;
        };
        let form = &self.account_form;
        if !form.can_submit(view, cx) {
            return;
        }
        let command = match form.page {
            AccountPage::Overview => return,
            AccountPage::Registration => AccountCommand::Register,
            AccountPage::Login | AccountPage::Credentials => {
                let username = form.username.read(cx).value().trim().to_owned();
                let password = form.password.read(cx).value().to_owned();
                if form.page == AccountPage::Login {
                    AccountCommand::Login { username, password }
                } else {
                    AccountCommand::Password { username, password }
                }
            }
            AccountPage::Nickname => AccountCommand::Nickname {
                account_id: view.status.account.clone(),
                nickname: form.nickname.read(cx).value().trim().into(),
            },
            AccountPage::Email(EmailPurpose::Registration, EmailStep::Code) => {
                AccountCommand::Verify {
                    code: form.code.read(cx).value().trim().into(),
                }
            }
            AccountPage::Email(EmailPurpose::Registration, EmailStep::Address) => {
                AccountCommand::Email {
                    email: form.email.read(cx).value().trim().into(),
                }
            }
            AccountPage::Email(EmailPurpose::Profile, EmailStep::Address) => {
                AccountCommand::RequestProfileEmail {
                    account_id: view.status.account.clone(),
                    email: form.email.read(cx).value().trim().into(),
                }
            }
            AccountPage::Email(EmailPurpose::Profile, EmailStep::Code) => {
                let Some(pending) = &view.status.pending_email else {
                    return;
                };
                AccountCommand::ConfirmProfileEmail {
                    account_id: view.status.account.clone(),
                    challenge: pending.challenge.id.clone(),
                    code: form.code.read(cx).value().trim().into(),
                }
            }
        };
        self.send_account_command(command);
    }

    fn send_account_command(&mut self, command: AccountCommand) {
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        self.account_form.pending = session.controller.account(command);
        self.account_form.feedback = if self.account_form.pending.is_some() {
            None
        } else {
            session
                .controller
                .model()
                .command_error
                .map(|error| Err(Failure::from(error)))
        };
    }
}

#[cfg(test)]
#[path = "account_tests.rs"]
mod tests;
