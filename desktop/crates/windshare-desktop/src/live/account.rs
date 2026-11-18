use super::Runtime;
use crate::{
    desktop::Desktop,
    localization::{Text, language},
    model::Overlay,
};
use gpui::{Entity, Subscription, Window, prelude::*};
use windshare_host::{AccountCommand, AccountState};
use windshare_ui::{Input, InputEvent};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AccountAction {
    ChooseLogin,
    ChoosePassword,
    ChooseEmail,
    Submit,
    SendEmail,
    Retry,
}
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum FormMode {
    Login,
    Password,
    Email,
}

pub struct AccountForm {
    pub(super) username: Entity<Input>,
    pub(super) password: Entity<Input>,
    pub(super) email: Entity<Input>,
    pub(super) code: Entity<Input>,
    pub(super) mode: FormMode,
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
                        this.account_action(AccountAction::Submit, cx);
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
            mode: FormMode::Login,
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
}
impl Desktop {
    pub fn open_account(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if let Runtime::Native(session) = &self.runtime
            && let Some(account) = &session.model.account
        {
            self.account_form.mode = if account.status.state == AccountState::EmailRequired {
                FormMode::Email
            } else if account.status.account.is_empty() {
                FormMode::Login
            } else {
                FormMode::Password
            };
            self.account_form.username.update(cx, |input, cx| {
                input.set_value(account.status.username.clone(), cx);
            });
            self.account_form
                .password
                .update(cx, |input, cx| input.set_value("", cx));
        }
        self.open_overlay(Overlay::Account, window, cx);
    }
    pub fn account_action(&mut self, action: AccountAction, cx: &mut Context<Self>) {
        let Runtime::Native(session) = &self.runtime else {
            return;
        };
        let Some(view) = &session.model.account else {
            return;
        };
        if view.busy {
            return;
        }
        let form = &mut self.account_form;
        let command = match action {
            AccountAction::ChooseLogin => {
                form.mode = FormMode::Login;
                None
            }
            AccountAction::ChoosePassword => {
                form.mode = FormMode::Password;
                None
            }
            AccountAction::ChooseEmail => {
                form.mode = FormMode::Email;
                None
            }
            AccountAction::Retry => Some(AccountCommand::Retry),
            AccountAction::SendEmail => Some(AccountCommand::Email {
                email: form.email.read(cx).value().trim().into(),
            }),
            AccountAction::Submit => {
                if form.mode == FormMode::Email {
                    Some(AccountCommand::Verify {
                        code: form.code.read(cx).value().trim().into(),
                    })
                } else {
                    let username = form.username.read(cx).value().trim().to_owned();
                    let password = form.password.read(cx).value().to_owned();
                    if username.is_empty() || password.is_empty() {
                        return;
                    }
                    form.password
                        .update(cx, |input, cx| input.set_value("", cx));
                    Some(if form.mode == FormMode::Login {
                        AccountCommand::Login { username, password }
                    } else {
                        AccountCommand::Password { username, password }
                    })
                }
            }
        };
        if let Some(command) = command {
            session.controller.account(command);
        }
        cx.notify();
    }
}
