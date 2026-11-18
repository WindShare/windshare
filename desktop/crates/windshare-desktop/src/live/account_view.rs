use super::{
    account::{AccountAction, AccountPage},
    network_settings::setting_row,
    projection::byte_size,
};
use crate::{
    desktop::Desktop,
    localization::{Language, Text, language},
    model::{Action, Page, ShareState},
};
use gpui::{Div, Entity, prelude::*, px};
use std::time::Duration;
use windshare_host::{AccountState, AccountView, Allowance};
use windshare_ui::{ButtonStyle, Icon, Input, Theme, Tone, column, row};

fn status(view: &AccountView, locale: Language) -> String {
    let value = &view.status;
    let text = match value.state {
        AccountState::PendingSetup if !value.active && value.account.is_empty() => {
            Text::AccountDormant
        }
        AccountState::PendingSetup if !value.account.is_empty() => Text::AccountRefreshing,
        AccountState::PendingSetup => Text::AccountSetup,
        AccountState::ProofOfWork => {
            let elapsed = Duration::from_nanos(value.proof_elapsed).as_secs();
            let label = locale.text(Text::AccountProof);
            return match locale {
                Language::English => format!("{label} {elapsed} s"),
                Language::SimplifiedChinese => format!("{label} {elapsed} 秒"),
            };
        }
        AccountState::EmailRequired => Text::AccountEmailNeeded,
        AccountState::Refreshing => Text::AccountRefreshing,
        AccountState::TemporarilyUnavailable => Text::AccountUnavailable,
        AccountState::Denied => Text::AccountDenied,
        AccountState::Ready => match value.allowance {
            Allowance::Spent => Text::AccountSpent,
            Allowance::Stale => Text::AccountStale,
            Allowance::Available => {
                return format!(
                    "{} · {}",
                    locale.text(Text::AccountRemaining),
                    byte_size(value.remaining_bytes)
                );
            }
        },
    };
    locale.text(text).into()
}

fn identity(view: &AccountView, locale: Language) -> Option<&str> {
    if !view.status.username.is_empty() {
        Some(&view.status.username)
    } else if !view.status.account.is_empty() {
        Some(locale.text(Text::DeviceAccount))
    } else {
        None
    }
}

impl Desktop {
    pub(super) fn account_setting(&self, theme: Theme, cx: &Context<Self>) -> Option<Div> {
        let view = self.account_view()?;
        let locale = language(cx);
        let status = status(view, locale);
        let detail = match identity(view, locale) {
            Some(name) => format!("{name} · {status}"),
            None => status,
        };
        Some(setting_row(
            theme,
            locale.text(Text::Account),
            detail,
            Self::button(
                "open-account",
                locale.text(AccountPage::entry(view).1),
                Icon::Shield,
                ButtonStyle::Secondary,
                Action::OpenAccount,
                cx,
            ),
        ))
    }

    pub fn account_notice(&self, theme: Theme, cx: &Context<Self>) -> Option<Div> {
        let view = self.account_view()?;
        if self.page == Page::Settings
            || !view.status.active
            || !self.shares.iter().any(|s| !s.finished())
            || view.status.state == AccountState::Ready
                && view.status.allowance == Allowance::Available
        {
            return None;
        }
        if matches!(
            view.status.state,
            AccountState::PendingSetup | AccountState::ProofOfWork
        ) && !self
            .shares
            .iter()
            .any(|s| matches!(s.state, ShareState::Preparing))
        {
            return None;
        }
        let locale = language(cx);
        Some(
            row()
                .gap(px(12.))
                .justify_between()
                .child(theme.caption(status(view, locale)))
                .child(
                    Self::button(
                        "account-attention",
                        locale.text(AccountPage::entry(view).1),
                        Icon::Shield,
                        ButtonStyle::Quiet,
                        Action::OpenAccount,
                        cx,
                    )
                    .compact(),
                ),
        )
    }

    pub fn account_title(&self) -> Text {
        match self.account_form.page {
            AccountPage::Overview => Text::Account,
            AccountPage::Login => Text::LoginExisting,
            AccountPage::Credentials
                if self
                    .account_view()
                    .is_some_and(|view| !view.status.username.is_empty()) =>
            {
                Text::EditAccountLogin
            }
            AccountPage::Credentials => Text::SetAccountLogin,
            AccountPage::VerifyEmail | AccountPage::RequestEmail => Text::AccountEmail,
        }
    }

    pub fn account_panel(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let Some(view) = self.account_view() else {
            return column();
        };
        let locale = language(cx);
        let mut panel = if self.account_form.page == AccountPage::Overview {
            self.account_overview(theme, view, cx)
        } else {
            self.account_editor(theme, view, cx)
        };
        if let Some(result) = &self.account_form.feedback {
            let (message, tone) = match result {
                Ok(text) => (locale.text(*text).to_owned(), Tone::Mint),
                Err(error) => {
                    let text = match error.code.as_str() {
                        "invalid_credentials" => Text::InvalidCredentials,
                        "username_taken" => Text::UsernameTaken,
                        "email_code_rejected" => Text::EmailCodeRejected,
                        "rate_limited" => Text::AccountRateLimited,
                        "invalid_account_input" => Text::InvalidAccountInput,
                        "account_service_unreachable" => Text::AccountUnavailable,
                        _ => {
                            return panel.child(
                                theme
                                    .caption(error.message.clone())
                                    .text_color(Tone::Rose.text(theme)),
                            );
                        }
                    };
                    (locale.text(text).to_owned(), Tone::Rose)
                }
            };
            panel = panel.child(theme.caption(message).text_color(tone.text(theme)));
        }
        panel
    }

    fn account_overview(&self, theme: Theme, view: &AccountView, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let busy = self.account_form.busy(view);
        let summary = column()
            .flex_1()
            .min_w_0()
            .gap(px(6.))
            .when_some(identity(view, locale), |col, name| {
                col.child(theme.heading(name.to_owned(), 19.))
            })
            .child(theme.caption(status(view, locale)))
            .when(
                view.status.state == AccountState::Ready && !view.reset_local.is_empty(),
                |col| {
                    col.child(theme.caption(format!(
                        "{} · {}",
                        locale.text(Text::AccountReset),
                        view.reset_local
                    )))
                },
            );
        let mut actions = row().flex_wrap().gap(px(8.));
        if view.status.state == AccountState::EmailRequired {
            actions = actions.child(
                Self::button(
                    "verify-account",
                    locale.text(Text::AccountEmail),
                    Icon::Shield,
                    ButtonStyle::Primary,
                    Action::Account(AccountAction::Open(AccountPage::VerifyEmail)),
                    cx,
                )
                .disabled(busy),
            );
        } else if matches!(
            view.status.state,
            AccountState::TemporarilyUnavailable | AccountState::Denied
        ) {
            actions = actions.child(
                Self::button(
                    "retry-account",
                    locale.text(Text::RetryConnection),
                    Icon::Refresh,
                    ButtonStyle::Secondary,
                    Action::Account(AccountAction::Retry),
                    cx,
                )
                .disabled(busy),
            );
        }
        if !view.status.account.is_empty() && view.status.state != AccountState::Denied {
            let label = if view.status.username.is_empty() {
                Text::SetAccountLogin
            } else {
                Text::EditAccountLogin
            };
            actions = actions.child(
                Self::button(
                    "set-account-login",
                    locale.text(label),
                    Icon::Lock,
                    ButtonStyle::Secondary,
                    Action::Account(AccountAction::Open(AccountPage::Credentials)),
                    cx,
                )
                .disabled(busy),
            );
        }
        let switch = if view.status.username.is_empty() {
            Text::LoginExisting
        } else {
            Text::SwitchAccount
        };
        column()
            .gap(px(20.))
            .child(
                row()
                    .gap(px(14.))
                    .child(theme.icon_tile(Icon::Shield, Tone::Mint, 44.))
                    .child(summary),
            )
            .child(
                actions.child(
                    Self::button(
                        "switch-account",
                        locale.text(switch),
                        Icon::Arrow,
                        ButtonStyle::Quiet,
                        Action::Account(AccountAction::Open(AccountPage::Login)),
                        cx,
                    )
                    .disabled(busy),
                ),
            )
    }

    fn account_editor(&self, theme: Theme, view: &AccountView, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let form = &self.account_form;
        let field = |input: &Entity<Input>, label| {
            column()
                .gap(px(7.))
                .child(theme.caption(locale.text(label)))
                .child(input.clone())
        };
        let mut panel = column().gap(px(16.));
        let mut actions = row().gap(px(8.)).flex_wrap();
        let submit = match form.page {
            AccountPage::Login | AccountPage::Credentials => {
                let login = form.page == AccountPage::Login;
                panel = panel
                    .child(theme.caption(locale.text(if login {
                        Text::AccountNewShares
                    } else {
                        Text::AccountPasswordDetail
                    })))
                    .child(field(
                        &form.username,
                        if login {
                            Text::AccountUsername
                        } else {
                            Text::CredentialUsername
                        },
                    ))
                    .child(field(
                        &form.password,
                        if login {
                            Text::AccountPasswordLabel
                        } else {
                            Text::CredentialPassword
                        },
                    ));
                if login {
                    Text::AccountLogin
                } else {
                    Text::SaveSettings
                }
            }
            AccountPage::VerifyEmail if form.entering_code(view) => {
                panel = panel
                    .child(theme.caption(locale.text(Text::AccountCheckInbox)))
                    .child(field(&form.code, Text::AccountCode));
                Text::AccountVerify
            }
            AccountPage::RequestEmail | AccountPage::VerifyEmail => {
                panel = panel
                    .child(theme.caption(locale.text(Text::AccountEmailNeeded)))
                    .child(field(&form.email, Text::AccountEmailLabel));
                Text::AccountSendCode
            }
            AccountPage::Overview => return panel,
        };
        actions = actions.child(
            Self::button(
                "account-submit",
                locale.text(if form.busy(view) {
                    Text::AccountWorking
                } else {
                    submit
                }),
                Icon::Arrow,
                ButtonStyle::Primary,
                Action::Account(AccountAction::Submit),
                cx,
            )
            .disabled(!form.can_submit(view, cx)),
        );
        if form.entering_code(view) {
            actions = actions.child(
                Self::button(
                    "new-email-code",
                    locale.text(Text::AccountRequestAnother),
                    Icon::Refresh,
                    ButtonStyle::Quiet,
                    Action::Account(AccountAction::Open(AccountPage::RequestEmail)),
                    cx,
                )
                .disabled(form.busy(view)),
            );
        }
        panel.child(actions.children(self.account_alternative(view, cx)))
    }

    fn account_alternative(
        &self,
        view: &AccountView,
        cx: &Context<Self>,
    ) -> Option<windshare_ui::Button> {
        let (label, page, icon) = match self.account_form.page {
            AccountPage::RequestEmail if view.status.email_code_pending => (
                Text::EnterExistingCode,
                AccountPage::VerifyEmail,
                Icon::Shield,
            ),
            AccountPage::Login | AccountPage::Credentials if !view.status.account.is_empty() => {
                (Text::BackToAccount, AccountPage::Overview, Icon::Shield)
            }
            AccountPage::VerifyEmail | AccountPage::RequestEmail => {
                (Text::LoginExisting, AccountPage::Login, Icon::Arrow)
            }
            _ => return None,
        };
        Some(
            Self::button(
                "account-alternative",
                language(cx).text(label),
                icon,
                ButtonStyle::Quiet,
                Action::Account(AccountAction::Open(page)),
                cx,
            )
            .disabled(self.account_form.busy(view)),
        )
    }
}
