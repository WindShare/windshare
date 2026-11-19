use super::{
    account::{AccountAction, AccountPage, EmailPurpose, EmailStep},
    network_settings::setting_row,
    projection::byte_size,
};
use crate::{
    desktop::Desktop,
    localization::{Language, Text, language},
    model::{Action, Page, ShareState},
};
use gpui::{Div, Entity, Role, prelude::*, px};
use windshare_host::{AccountState, AccountView, Allowance};
use windshare_ui::{ButtonStyle, Icon, Input, Theme, Tone, column, row};

fn status(view: &AccountView, locale: Language) -> String {
    let value = &view.status;
    let text = match value.state {
        AccountState::PendingSetup if !value.account.is_empty() => Text::AccountRefreshing,
        AccountState::PendingSetup | AccountState::RegistrationRequired => Text::AccountDormant,
        AccountState::Registering { .. } => Text::AccountSetup,
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
    if let Some(nickname) = &view.status.profile.nickname {
        return Some(&nickname.value);
    }
    if !view.status.username.is_empty() {
        Some(&view.status.username)
    } else if !view.status.account.is_empty() {
        Some(locale.text(Text::DeviceAccount))
    } else {
        None
    }
}

impl AccountPage {
    const fn title(self, view: &AccountView) -> Text {
        match self {
            Self::Overview | Self::Registration => Text::Account,
            Self::Login => Text::LoginExisting,
            Self::Credentials if view.status.username.is_empty() => Text::SetAccountLogin,
            Self::Credentials => Text::EditAccountLogin,
            Self::Nickname if view.status.profile.nickname.is_none() => Text::SetNickname,
            Self::Nickname => Text::EditNickname,
            Self::Email(EmailPurpose::Profile, EmailStep::Address)
                if view.status.profile.email.is_empty() =>
            {
                Text::SetProfileEmail
            }
            Self::Email(EmailPurpose::Profile, EmailStep::Address) => Text::EditProfileEmail,
            Self::Email(_, _) => Text::AccountEmail,
        }
    }
}

impl Desktop {
    pub(super) fn account_setting(&self, theme: Theme, cx: &Context<Self>) -> Option<Div> {
        let view = self.account_view()?;
        let locale = language(cx);
        let status = if self.account_form.page == AccountPage::Registration
            && matches!(self.account_form.feedback, Some(Err(_)))
            && view.status.state == AccountState::RegistrationRequired
        {
            locale.text(Text::AccountRegistrationFailed).to_owned()
        } else {
            status(view, locale)
        };
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
            AccountState::PendingSetup
                | AccountState::RegistrationRequired
                | AccountState::Registering { .. }
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
        self.account_view()
            .map_or(Text::Account, |view| self.account_form.page.title(view))
    }

    pub fn account_panel(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let Some(view) = self.account_view() else {
            return column();
        };
        let locale = language(cx);
        let mut panel = match self.account_form.page {
            AccountPage::Registration => self.registration_panel(theme, view, cx),
            AccountPage::Overview => self.account_overview(theme, view, cx),
            _ => self.account_editor(theme, view, cx),
        };
        if let Some(result) = &self.account_form.feedback {
            let (message, tone) = match result {
                Ok(text) => (locale.text(*text).to_owned(), Tone::Mint),
                Err(error) => {
                    let text = match error.code.as_str() {
                        "invalid_credentials" => Text::InvalidCredentials,
                        "username_taken" => Text::UsernameTaken,
                        "invalid_nickname" => Text::InvalidNickname,
                        "nickname_cooldown" => Text::NicknameInterval,
                        "invalid_email" => Text::InvalidEmail,
                        "unsupported_email" => Text::UnsupportedEmail,
                        "email_delivery_unavailable" => Text::EmailDeliveryUnavailable,
                        "account_changed" => Text::AccountChanged,
                        "account_denied" => Text::AccountDenied,
                        "email_code_incorrect" => Text::EmailCodeIncorrect,
                        "email_code_rejected" => Text::EmailCodeRejected,
                        "rate_limited" => Text::AccountRateLimited,
                        "invalid_account_input" => Text::InvalidAccountInput,
                        "account_service_unreachable" => Text::AccountUnavailable,
                        _ if matches!(
                            self.account_form.page,
                            AccountPage::Registration
                                | AccountPage::Email(EmailPurpose::Registration, _)
                        ) =>
                        {
                            Text::AccountRegistrationFailed
                        }
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
            panel = panel.child(
                theme
                    .caption(message.clone())
                    .id("account-feedback")
                    .role(if result.is_err() {
                        Role::Alert
                    } else {
                        Role::Status
                    })
                    .aria_label(message)
                    .text_color(tone.text(theme)),
            );
        }
        panel
    }

    fn account_overview(&self, theme: Theme, view: &AccountView, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let busy = self.account_form.busy(view);
        let locked_until = view.nickname_locked_until();
        let summary = column()
            .flex_1()
            .min_w_0()
            .gap(px(6.))
            .when_some(identity(view, locale), |col, name| {
                col.child(theme.heading(name.to_owned(), 19.).truncate())
            })
            .child(theme.caption(status(view, locale)))
            .when_some(locked_until, |col, at| {
                col.child(theme.caption(locale.nickname_next_change(at)))
            })
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
        column()
            .gap(px(20.))
            .child(
                row()
                    .gap(px(14.))
                    .child(theme.icon_tile(Icon::Shield, Tone::Mint, 44.))
                    .child(summary)
                    .when(view.can_edit_profile(), |row| {
                        row.child(
                            Self::button(
                                "account-nickname",
                                locale.text(AccountPage::Nickname.title(view)),
                                Icon::Sliders,
                                ButtonStyle::Quiet,
                                Action::Account(AccountAction::Open(AccountPage::Nickname)),
                                cx,
                            )
                            .compact()
                            .disabled(busy || locked_until.is_some()),
                        )
                    }),
            )
            .when(view.can_edit_profile(), |panel| {
                let email = &view.status.profile.email;
                let page = EmailPurpose::Profile.entry(view);
                panel.child(theme.separator()).child(setting_row(
                    theme,
                    locale.text(Text::AccountEmailLabel),
                    if email.is_empty() {
                        locale.text(Text::EmailNotSet).to_owned()
                    } else {
                        email.clone()
                    },
                    Self::button(
                        "account-profile-email",
                        locale.text(page.title(view)),
                        Icon::Chevron,
                        ButtonStyle::Secondary,
                        Action::Account(AccountAction::Open(page)),
                        cx,
                    )
                    .disabled(busy),
                ))
            })
            .child(self.account_access_actions(view, cx))
    }

    fn account_access_actions(&self, view: &AccountView, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let busy = self.account_form.busy(view);
        let mut actions = row().flex_wrap().gap(px(8.));
        if view.status.state == AccountState::EmailRequired {
            actions = actions.child(
                Self::button(
                    "verify-account",
                    locale.text(Text::AccountEmail),
                    Icon::Shield,
                    ButtonStyle::Primary,
                    Action::Account(AccountAction::Open(EmailPurpose::Registration.entry(view))),
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
        if view.can_edit_profile() {
            actions = actions.child(
                Self::button(
                    "set-account-login",
                    locale.text(AccountPage::Credentials.title(view)),
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
            AccountPage::Nickname => {
                panel = panel
                    .child(field(&form.nickname, Text::NicknameField))
                    .child(theme.caption(locale.text(Text::NicknameInterval)));
                Text::SaveSettings
            }
            AccountPage::Email(purpose, EmailStep::Code) => {
                let prompt = if purpose == EmailPurpose::Profile
                    && let Some(pending) = &view.status.pending_email
                {
                    locale.code_sent_to(&pending.address)
                } else {
                    locale.text(Text::AccountCheckInbox).into()
                };
                panel = panel
                    .child(theme.caption(prompt))
                    .child(field(&form.code, Text::AccountCode));
                Text::AccountVerify
            }
            AccountPage::Email(purpose, EmailStep::Address) => {
                panel = panel
                    .when(purpose == EmailPurpose::Registration, |panel| {
                        panel.child(theme.caption(locale.text(Text::AccountEmailNeeded)))
                    })
                    .child(field(&form.email, Text::AccountEmailLabel))
                    .child(theme.caption(locale.text(Text::SupportedEmailProviders)));
                Text::AccountSendCode
            }
            AccountPage::Overview | AccountPage::Registration => return panel,
        };
        panel.child(self.account_editor_actions(view, submit, cx))
    }

    fn account_editor_actions(&self, view: &AccountView, submit: Text, cx: &Context<Self>) -> Div {
        let form = &self.account_form;
        let locale = language(cx);
        let mut actions = row().gap(px(8.)).flex_wrap().child(
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
        if let AccountPage::Email(purpose, EmailStep::Code) = form.page {
            actions = actions.child(
                Self::button(
                    "new-email-code",
                    locale.text(Text::AccountRequestAnother),
                    Icon::Refresh,
                    ButtonStyle::Quiet,
                    Action::Account(AccountAction::Open(AccountPage::Email(
                        purpose,
                        EmailStep::Address,
                    ))),
                    cx,
                )
                .disabled(form.busy(view)),
            );
        }
        actions.children(self.account_alternative(view, cx))
    }

    fn account_alternative(
        &self,
        view: &AccountView,
        cx: &Context<Self>,
    ) -> Option<windshare_ui::Button> {
        let (label, page, icon) = match self.account_form.page {
            AccountPage::Email(purpose, EmailStep::Address) if purpose.has_code(view) => (
                Text::EnterExistingCode,
                AccountPage::Email(purpose, EmailStep::Code),
                Icon::Shield,
            ),
            AccountPage::Login
            | AccountPage::Credentials
            | AccountPage::Nickname
            | AccountPage::Email(EmailPurpose::Profile, _)
                if !view.status.account.is_empty() =>
            {
                (Text::BackToAccount, AccountPage::Overview, Icon::Shield)
            }
            AccountPage::Email(EmailPurpose::Registration, _) => {
                (Text::LoginExisting, AccountPage::Login, Icon::Arrow)
            }
            AccountPage::Login if view.status.account.is_empty() => {
                (Text::SetUpAccount, AccountPage::entry(view).0, Icon::Shield)
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
