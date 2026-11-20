use super::{
    account::{AccountAction, AccountFormError, AccountPage, EmailPurpose, EmailStep},
    projection::byte_size,
};
use crate::{
    desktop::Desktop,
    flow_state::ShareFlow,
    localization::{Language, Text, language},
    model::{Action, Overlay, Page, SettingsSection, Share, ShareState},
};
use gpui::{Div, Entity, Role, prelude::*, px};
use windshare_host::{AccountState, AccountStatus, AccountView, Allowance, ShareAuthorization};
use windshare_ui::{ButtonStyle, Icon, Input, Theme, Tone, column, row};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum AccountNoticeSurface {
    AccountSettings,
    OfficialAuthorizationCard,
    Ambient,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
struct ActiveShareDemand {
    preparing: bool,
    official_auth_wait: bool,
}

const fn is_official_authorization_wait(share: &Share) -> bool {
    matches!(share.state, ShareState::AwaitingAuthorization)
        && matches!(share.authorization, Some(ShareAuthorization::Official))
}

const fn should_show_account_notice(
    status: &AccountStatus,
    surface: AccountNoticeSurface,
    active_shares: Option<ActiveShareDemand>,
) -> bool {
    if !status.active || !matches!(surface, AccountNoticeSurface::Ambient) {
        return false;
    }
    let Some(demand) = active_shares else {
        return false;
    };
    match status.state {
        // Silent background setup and routine ticket/allowance renewal settle
        // quickly on their own; flashing an in-flow notice jolts the share stage.
        AccountState::PendingSetup | AccountState::RefreshRequired | AccountState::Refreshing => {
            false
        }
        AccountState::Ready => matches!(status.allowance, Allowance::Spent),
        // Anonymous or independent relays may already publish the share; only
        // surface a registration or email prompt while a share is blocked on it.
        AccountState::RegistrationRequired | AccountState::EmailRequired => {
            demand.official_auth_wait
        }
        AccountState::Registering { .. } => demand.preparing,
        AccountState::TemporarilyUnavailable | AccountState::Denied => true,
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

    fn account_notice_surface(&self) -> AccountNoticeSurface {
        if self.page == Page::Settings && self.settings_section == SettingsSection::Account {
            return AccountNoticeSurface::AccountSettings;
        }
        if self.page == Page::Share
            && matches!(self.share_flow, ShareFlow::Running(run) if self
                .shares
                .iter()
                .find(|share| share.id == run.share_id)
                .is_some_and(is_official_authorization_wait))
        {
            return AccountNoticeSurface::OfficialAuthorizationCard;
        }
        AccountNoticeSurface::Ambient
    }

    fn active_share_demand(&self) -> Option<ActiveShareDemand> {
        let mut any_unfinished = false;
        let mut demand = ActiveShareDemand::default();
        for share in &self.shares {
            if share.finished() {
                continue;
            }
            any_unfinished = true;
            demand.preparing |= matches!(share.state, ShareState::Preparing);
            demand.official_auth_wait |= is_official_authorization_wait(share);
        }
        any_unfinished.then_some(demand)
    }

    pub fn account_notice(&self, theme: Theme, cx: &Context<Self>) -> Option<Div> {
        let view = self.account_view()?;
        if !should_show_account_notice(
            &view.status,
            self.account_notice_surface(),
            self.active_share_demand(),
        ) {
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
        let can_submit = form.can_submit(view, cx);
        form.rendered_can_submit.set(Some(can_submit));
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
            .disabled(!can_submit),
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

#[cfg(test)]
#[path = "account_view_tests.rs"]
mod tests;
