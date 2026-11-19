use super::Runtime;
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::Overlay,
};
use gpui::{Entity, FocusHandle, Focusable, Subscription, Window, prelude::*};
use windshare_host::{AccountCommand, AccountOutcome, AccountState, AccountView, Failure, Model};
use windshare_ui::{Input, InputEvent};

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
    fn map_target<T>(&self, map: impl FnOnce(&Target) -> T) -> AccountScreen<T> {
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

    const fn target(&self) -> Option<&Target> {
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
    pub(super) const fn has_code(&self, view: &AccountView) -> bool {
        match self {
            Self::Registration => view.status.email_code_pending,
            Self::Profile(_) => view.status.pending_email.is_some(),
        }
    }

    const fn step(&self, view: &AccountView) -> EmailStep {
        if self.has_code(view) {
            EmailStep::Code
        } else {
            EmailStep::Address
        }
    }

    pub(super) const fn entry(self, view: &AccountView) -> AccountScreen<Target> {
        let step = self.step(view);
        AccountScreen::Email(self, step)
    }
}

impl<Target> AccountScreen<Target> {
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
}

#[derive(Clone, Copy, Debug)]
pub enum AccountAction {
    Open(AccountPage),
    Submit,
    Retry,
}

pub(super) enum AccountFormError {
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

pub struct AccountForm {
    pub(super) nickname: Entity<Input>,
    pub(super) username: Entity<Input>,
    pub(super) password: Entity<Input>,
    pub(super) email: Entity<Input>,
    pub(super) code: Entity<Input>,
    page: AccountScreen<String>,
    pending: Option<String>,
    pub(super) feedback: Option<Result<Text, AccountFormError>>,
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
            page: AccountScreen::Overview,
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

    pub(super) fn page(&self) -> AccountPage {
        self.page.map_target(|_| ())
    }

    fn target_changed(&self, view: &AccountView) -> bool {
        self.page
            .target()
            .is_some_and(|target| target != &view.status.account)
    }

    pub(super) fn field_focus(&self, model: &Model, cx: &Context<Desktop>) -> Option<FocusHandle> {
        model.account.as_ref()?;
        let input = match self.page {
            AccountScreen::Login | AccountScreen::Credentials(_) => &self.username,
            AccountScreen::Nickname(_) => &self.nickname,
            AccountScreen::Email(_, EmailStep::Code) => &self.code,
            AccountScreen::Email(_, EmailStep::Address) => &self.email,
            AccountScreen::Overview | AccountScreen::Registration => return None,
        };
        Some(input.read(cx).focus_handle(cx))
    }

    pub(super) const fn busy(&self, view: &AccountView) -> bool {
        self.pending.is_some() || view.busy()
    }

    pub(super) fn can_submit(&self, view: &AccountView, cx: &Context<Desktop>) -> bool {
        if self.busy(view) || self.target_changed(view) {
            return false;
        }
        match self.page() {
            AccountPage::Overview => false,
            AccountPage::Registration => {
                view.status.account.is_empty()
                    && !matches!(
                        view.status.state,
                        AccountState::Registering { .. } | AccountState::Denied
                    )
            }
            AccountPage::Login | AccountPage::Credentials(()) => {
                !self.username.read(cx).value().trim().is_empty()
                    && !self.password.read(cx).value().is_empty()
            }
            AccountPage::Nickname(()) => {
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
                    EmailPurpose::Profile(()) => view.can_edit_profile(),
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

    pub(super) fn reconcile(&mut self, model: &Model, cx: &mut Context<Desktop>) {
        let Some(view) = &model.account else { return };
        if self.target_changed(view) {
            self.close_stale_edit(cx);
            return;
        }
        if let Some(id) = &self.pending {
            if let Some(op) = view.operation.as_ref().filter(|op| &op.id == id) {
                match &op.outcome {
                    AccountOutcome::Running => return,
                    AccountOutcome::Failed { error } => {
                        let error = AccountFormError::from(error.clone());
                        if self.page.target().is_some()
                            && matches!(error, AccountFormError::AccountChanged)
                        {
                            self.close_stale_edit(cx);
                            return;
                        }
                        self.feedback = Some(Err(error));
                    }
                    AccountOutcome::Succeeded => self.completed(view, cx),
                }
            } else if let Some(error) = model.settings_rejection(id) {
                self.feedback = Some(Err(error.clone().into()));
            } else {
                return;
            }
            self.pending = None;
        }
        match &mut self.page {
            AccountScreen::Registration | AccountScreen::Email(EmailPurpose::Registration, _)
                if !view.status.account.is_empty() =>
            {
                self.page = AccountScreen::Overview;
                self.feedback = None;
                self.clear_secrets(cx);
            }
            AccountScreen::Registration if view.status.state == AccountState::EmailRequired => {
                self.page = EmailPurpose::Registration.entry(view);
            }
            AccountScreen::Email(EmailPurpose::Registration, _)
                if view.status.state != AccountState::EmailRequired =>
            {
                self.page = AccountScreen::entry(view).0;
            }
            AccountScreen::Nickname(_) | AccountScreen::Email(EmailPurpose::Profile(_), _)
                if !view.can_edit_profile() =>
            {
                self.page = AccountScreen::Overview;
            }
            AccountScreen::Nickname(_) if view.nickname_locked_until().is_some() => {
                self.page = AccountScreen::Overview;
            }
            AccountScreen::Email(purpose, step @ EmailStep::Code) if !purpose.has_code(view) => {
                *step = EmailStep::Address;
            }
            _ => {}
        }
    }

    fn close_stale_edit(&mut self, cx: &mut Context<Desktop>) {
        self.page = AccountScreen::Overview;
        self.pending = None;
        self.feedback = Some(Err(AccountFormError::AccountChanged));
        self.clear_secrets(cx);
    }

    fn clear_secrets(&self, cx: &mut Context<Desktop>) {
        self.password
            .update(cx, |input, cx| input.set_value("", cx));
        self.code.update(cx, |input, cx| input.set_value("", cx));
    }

    fn completed(&mut self, view: &AccountView, cx: &mut Context<Desktop>) {
        let message = match &mut self.page {
            AccountScreen::Registration if view.status.state == AccountState::EmailRequired => {
                self.page = EmailPurpose::Registration.entry(view);
                return;
            }
            AccountScreen::Registration => Text::AccountCreated,
            AccountScreen::Login => Text::AccountSignedIn,
            AccountScreen::Credentials(_) => Text::AccountLoginSaved,
            AccountScreen::Nickname(_) => Text::NicknameSaved,
            AccountScreen::Email(purpose, step @ EmailStep::Address) => {
                *step = purpose.step(view);
                self.code.update(cx, |input, cx| input.set_value("", cx));
                return;
            }
            AccountScreen::Email(EmailPurpose::Registration, EmailStep::Code) => {
                Text::AccountVerified
            }
            AccountScreen::Email(EmailPurpose::Profile(_), EmailStep::Code) => {
                Text::ProfileEmailSaved
            }
            AccountScreen::Overview => return,
        };
        self.clear_secrets(cx);
        self.page = AccountScreen::Overview;
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
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        let Some(view) = session.model.account.as_ref() else {
            return;
        };
        let page = AccountPage::entry(view).0;
        session.controller.reload_account_selection();
        self.open_overlay(Overlay::Account, window, cx);
        if self.account_form.pending.is_none()
            && !(page == AccountPage::Registration && self.account_form.page() == page)
        {
            self.set_account_page(page, window, cx);
        }
    }

    pub(crate) fn sync_account_view_visibility(&self, window: &Window) {
        if let Runtime::Native(session) = &self.runtime {
            session.controller.set_account_view_visible(
                window.is_visible()
                    && self
                        .overlays
                        .last()
                        .is_some_and(|frame| matches!(frame.content, Overlay::Account))
                    && self.account_form.page() == AccountPage::Overview
                    && self
                        .account_view()
                        .is_some_and(|view| !view.status.account.is_empty()),
            );
        }
    }

    fn set_account_page(&mut self, page: AccountPage, window: &mut Window, cx: &mut Context<Self>) {
        let Some(view) = self.account_view() else {
            return;
        };
        if page.target().is_some()
            && (view.status.account.is_empty() || self.account_form.target_changed(view))
        {
            return;
        }
        let username = if page == AccountPage::Credentials(()) {
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
            AccountPage::Email(EmailPurpose::Profile(()), EmailStep::Address) => Some(
                view.status
                    .pending_email
                    .as_ref()
                    .map_or(&view.status.profile.email, |pending| &pending.address)
                    .clone(),
            ),
            AccountPage::Email(EmailPurpose::Registration, EmailStep::Address)
                if !matches!(
                    self.account_form.page(),
                    AccountPage::Email(EmailPurpose::Registration, _)
                ) =>
            {
                Some(String::new())
            }
            _ => None,
        };
        let target_page = page.map_target(|()| view.status.account.clone());
        let form = &mut self.account_form;
        form.page = target_page;
        if page == AccountPage::Nickname(()) {
            form.nickname
                .update(cx, |input, cx| input.set_value(nickname, cx));
        }
        if let Some(email) = email {
            form.email
                .update(cx, |input, cx| input.set_value(email, cx));
        }
        form.feedback = None;
        form.clear_secrets(cx);
        form.username
            .update(cx, |input, cx| input.set_value(username, cx));
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
                if self.account_form.page() == AccountPage::Registration
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
        let command = match &form.page {
            AccountScreen::Overview => return,
            AccountScreen::Registration => AccountCommand::Register,
            AccountScreen::Login | AccountScreen::Credentials(_) => {
                let username = form.username.read(cx).value().trim().to_owned();
                let password = form.password.read(cx).value().to_owned();
                if let AccountScreen::Credentials(account_id) = &form.page {
                    AccountCommand::Password {
                        account_id: account_id.clone(),
                        username,
                        password,
                    }
                } else {
                    AccountCommand::Login { username, password }
                }
            }
            AccountScreen::Nickname(account_id) => AccountCommand::Nickname {
                account_id: account_id.clone(),
                nickname: form.nickname.read(cx).value().trim().into(),
            },
            AccountScreen::Email(EmailPurpose::Registration, EmailStep::Code) => {
                AccountCommand::Verify {
                    code: form.code.read(cx).value().trim().into(),
                }
            }
            AccountScreen::Email(EmailPurpose::Registration, EmailStep::Address) => {
                AccountCommand::Email {
                    email: form.email.read(cx).value().trim().into(),
                }
            }
            AccountScreen::Email(EmailPurpose::Profile(account_id), EmailStep::Address) => {
                AccountCommand::RequestProfileEmail {
                    account_id: account_id.clone(),
                    email: form.email.read(cx).value().trim().into(),
                }
            }
            AccountScreen::Email(EmailPurpose::Profile(account_id), EmailStep::Code) => {
                let Some(pending) = &view.status.pending_email else {
                    return;
                };
                AccountCommand::ConfirmProfileEmail {
                    account_id: account_id.clone(),
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
                .map(|error| Err(Failure::from(error).into()))
        };
    }
}
