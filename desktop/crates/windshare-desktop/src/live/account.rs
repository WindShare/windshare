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
    Login,
    Credentials,
    VerifyEmail,
    RequestEmail,
}

impl AccountPage {
    pub(super) const fn entry(view: &AccountView) -> (Self, Text) {
        match view.status.state {
            AccountState::EmailRequired => (Self::VerifyEmail, Text::AccountEmail),
            AccountState::PendingSetup if !view.status.active && view.status.account.is_empty() => {
                (Self::Login, Text::LoginExisting)
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

pub struct AccountForm {
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
        let username = cx.new(|cx| Input::new(locale.text(Text::AccountUsername), cx));
        let password =
            cx.new(|cx| Input::new(locale.text(Text::AccountPasswordLabel), cx).password());
        let email = cx.new(|cx| Input::new(locale.text(Text::AccountEmailLabel), cx));
        let code = cx.new(|cx| Input::new(locale.text(Text::AccountCode), cx));
        let subscriptions = [&username, &password, &email, &code]
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
            (&self.username, Text::AccountUsername),
            (&self.password, Text::AccountPasswordLabel),
            (&self.email, Text::AccountEmailLabel),
            (&self.code, Text::AccountCode),
        ] {
            input.update(cx, |input, cx| input.set_labels(locale.text(text), "", cx));
        }
    }

    pub(super) fn field_focus(&self, model: &Model, cx: &Context<Desktop>) -> Option<FocusHandle> {
        let view = model.account.as_ref()?;
        let input = match self.page {
            AccountPage::Login | AccountPage::Credentials => &self.username,
            AccountPage::VerifyEmail if self.entering_code(view) => &self.code,
            AccountPage::RequestEmail | AccountPage::VerifyEmail => &self.email,
            AccountPage::Overview => return None,
        };
        Some(input.read(cx).focus_handle(cx))
    }

    pub(super) const fn busy(&self, view: &AccountView) -> bool {
        self.pending.is_some() || view.busy()
    }

    pub(super) const fn entering_code(&self, view: &AccountView) -> bool {
        matches!(self.page, AccountPage::VerifyEmail) && view.status.email_code_pending
    }

    pub(super) fn can_submit(&self, view: &AccountView, cx: &Context<Desktop>) -> bool {
        if self.busy(view) {
            return false;
        }
        match self.page {
            AccountPage::Overview => false,
            AccountPage::Credentials if view.status.account.is_empty() => false,
            AccountPage::Login | AccountPage::Credentials => {
                !self.username.read(cx).value().trim().is_empty()
                    && !self.password.read(cx).value().is_empty()
            }
            AccountPage::VerifyEmail | AccountPage::RequestEmail => {
                view.status.state == AccountState::EmailRequired
                    && if self.entering_code(view) {
                        !self.code.read(cx).value().trim().is_empty()
                    } else {
                        !self.email.read(cx).value().trim().is_empty()
                    }
            }
        }
    }

    pub(super) fn reconcile(&mut self, model: &Model, cx: &mut Context<Desktop>) {
        let Some(view) = &model.account else { return };
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
        // A CLI login or a background registration can resolve a challenge while the dialog is open.
        if matches!(
            self.page,
            AccountPage::VerifyEmail | AccountPage::RequestEmail
        ) && view.status.state != AccountState::EmailRequired
        {
            self.page = AccountPage::Overview;
        }
    }

    fn completed(&mut self, view: &AccountView, cx: &mut Context<Desktop>) {
        let message = match self.page {
            AccountPage::Login => Text::AccountSignedIn,
            AccountPage::Credentials => Text::AccountLoginSaved,
            AccountPage::RequestEmail | AccountPage::VerifyEmail
                if view.status.state == AccountState::EmailRequired =>
            {
                self.page = AccountPage::VerifyEmail;
                self.code.update(cx, |input, cx| input.set_value("", cx));
                return;
            }
            AccountPage::VerifyEmail | AccountPage::RequestEmail => Text::AccountVerified,
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
        if self.account_form.pending.is_none() {
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
        let form = &mut self.account_form;
        form.page = page;
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
            AccountAction::Submit => self.submit_account(cx),
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
            AccountPage::Login | AccountPage::Credentials => {
                let username = form.username.read(cx).value().trim().to_owned();
                let password = form.password.read(cx).value().to_owned();
                if form.page == AccountPage::Login {
                    AccountCommand::Login { username, password }
                } else {
                    AccountCommand::Password { username, password }
                }
            }
            AccountPage::VerifyEmail if form.entering_code(view) => AccountCommand::Verify {
                code: form.code.read(cx).value().trim().into(),
            },
            AccountPage::RequestEmail | AccountPage::VerifyEmail => AccountCommand::Email {
                email: form.email.read(cx).value().trim().into(),
            },
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
