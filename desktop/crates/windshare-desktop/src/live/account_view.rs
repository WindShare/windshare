use super::{
    account::{AccountAction, AccountFormError, AccountPage, EmailPurpose, EmailStep},
    projection::byte_size,
};
use crate::{
    desktop::Desktop,
    localization::{Language, Text, language},
    model::{Action, Overlay, Page, SettingsSection, ShareState},
};
use gpui::{Div, Entity, Role, prelude::*, px};
use windshare_host::{AccountState, AccountView, Allowance};
use windshare_ui::{ButtonStyle, Icon, Input, Theme, Tone, column, row};

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

impl Desktop {
    pub(crate) fn account_status_summary(view: &AccountView, locale: Language) -> String {
        let value = &view.status;
        let text = match value.state {
            AccountState::PendingSetup | AccountState::RegistrationRequired => Text::AccountDormant,
            AccountState::Registering { .. } => Text::AccountSetup,
            AccountState::EmailRequired => Text::AccountEmailNeeded,
            AccountState::RefreshRequired => Text::AccountRefreshRequired,
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

    pub fn account_notice(&self, theme: Theme, cx: &Context<Self>) -> Option<Div> {
        let view = self.account_view()?;
        if (self.page == Page::Settings && self.settings_section == SettingsSection::Account)
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
                .child(theme.caption(Self::account_status_summary(view, locale)))
                .child(
                    Self::button(
                        "account-attention",
                        locale.text(AccountPage::entry(view).1),
                        Icon::User,
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
            .map_or(Text::Account, |view| self.account_form.page().title(view))
    }

    pub fn account_panel(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let Some(view) = self.account_view() else {
            return column();
        };
        let mut panel = match self.account_form.page() {
            AccountPage::Registration => self.registration_panel(theme, view, cx),
            AccountPage::Overview => self.account_identity_content(theme, view, cx),
            _ => self.account_editor(theme, view, cx),
        };
        if let Some(banner) = self.account_feedback_banner(theme, cx) {
            panel = panel.child(banner);
        }
        panel
    }

    pub(crate) fn account_feedback_banner(
        &self,
        theme: Theme,
        cx: &Context<Self>,
    ) -> Option<gpui::Stateful<Div>> {
        let result = self.account_form.feedback.as_ref()?;
        let locale = language(cx);
        let (message, tone) = match result {
            Ok(text) => (locale.text(*text).to_owned(), Tone::Mint),
            Err(AccountFormError::AccountChanged) => {
                (locale.text(Text::AccountChanged).to_owned(), Tone::Rose)
            }
            Err(AccountFormError::Command(error)) => {
                let text = match error.code.as_str() {
                    "invalid_credentials" => Some(Text::InvalidCredentials),
                    "username_taken" => Some(Text::UsernameTaken),
                    "email_taken" => Some(Text::EmailTaken),
                    "invalid_nickname" => Some(Text::InvalidNickname),
                    "nickname_cooldown" => Some(Text::NicknameInterval),
                    "invalid_email" => Some(Text::InvalidEmail),
                    "unsupported_email" => Some(Text::UnsupportedEmail),
                    "email_delivery_unavailable" => Some(Text::EmailDeliveryUnavailable),
                    "account_denied" => Some(Text::AccountDenied),
                    "email_code_incorrect" => Some(Text::EmailCodeIncorrect),
                    "email_code_rejected" => Some(Text::EmailCodeRejected),
                    "rate_limited" => Some(Text::AccountRateLimited),
                    "invalid_account_input" => Some(Text::InvalidAccountInput),
                    "account_service_unreachable" => Some(Text::AccountUnavailable),
                    _ if matches!(
                        self.account_form.page(),
                        AccountPage::Registration
                            | AccountPage::Email(EmailPurpose::Registration, _)
                    ) =>
                    {
                        Some(Text::AccountRegistrationFailed)
                    }
                    _ => None,
                };
                let text =
                    text.map_or_else(|| error.message.clone(), |key| locale.text(key).to_owned());
                (text, Tone::Rose)
            }
        };
        Some(
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
        let submit = match form.page() {
            AccountPage::Login | AccountPage::Credentials(()) => {
                let login = form.page() == AccountPage::Login;
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
            AccountPage::Nickname(()) => {
                panel = panel
                    .child(field(&form.nickname, Text::NicknameField))
                    .child(theme.caption(locale.text(Text::NicknameInterval)));
                Text::SaveSettings
            }
            AccountPage::Email(purpose, EmailStep::Code) => {
                let prompt = if purpose == EmailPurpose::Profile(())
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
        let submit_icon = match submit {
            Text::SaveSettings | Text::AccountVerify => Icon::Check,
            _ => Icon::Arrow,
        };
        let mut actions = row().gap(px(8.)).flex_wrap().child(
            Self::button(
                "account-submit",
                locale.text(if form.busy(view) {
                    Text::AccountWorking
                } else {
                    submit
                }),
                submit_icon,
                ButtonStyle::Primary,
                Action::Account(AccountAction::Submit),
                cx,
            )
            .disabled(!form.can_submit(view, cx)),
        );
        if let AccountPage::Email(purpose, EmailStep::Code) = form.page() {
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
        let from_settings = self.page == Page::Settings
            && self.settings_section == SettingsSection::Account
            && self
                .overlays
                .last()
                .is_some_and(|frame| matches!(frame.content, Overlay::Account));
        let (label, action, icon) = match self.account_form.page() {
            AccountPage::Email(purpose, EmailStep::Address) if purpose.has_code(view) => (
                Text::EnterExistingCode,
                Action::Account(AccountAction::Open(AccountPage::Email(
                    purpose,
                    EmailStep::Code,
                ))),
                Icon::Shield,
            ),
            AccountPage::Login
            | AccountPage::Credentials(())
            | AccountPage::Nickname(())
            | AccountPage::Email(EmailPurpose::Profile(()), _)
                if from_settings =>
            {
                (Text::Cancel, Action::Close, Icon::Close)
            }
            AccountPage::Login
            | AccountPage::Credentials(())
            | AccountPage::Nickname(())
            | AccountPage::Email(EmailPurpose::Profile(()), _)
                if !view.status.account.is_empty() =>
            {
                (
                    Text::BackToAccount,
                    Action::Account(AccountAction::Open(AccountPage::Overview)),
                    Icon::ArrowLeft,
                )
            }
            AccountPage::Email(EmailPurpose::Registration, _) => (
                Text::LoginExisting,
                Action::Account(AccountAction::Open(AccountPage::Login)),
                Icon::Arrow,
            ),
            AccountPage::Login if view.status.account.is_empty() => (
                Text::SetUpAccount,
                Action::Account(AccountAction::Open(AccountPage::entry(view).0)),
                Icon::User,
            ),
            _ => return None,
        };
        Some(
            Self::button(
                "account-alternative",
                language(cx).text(label),
                icon,
                ButtonStyle::Quiet,
                action,
                cx,
            )
            .disabled(self.account_form.busy(view)),
        )
    }
}
