use super::{
    AccountDialog, AccountDialogEvent, AccountFormError, AccountOrigin, AccountPage, EmailPurpose,
    EmailStep, registration::registration_panel_view, settings::account_identity_view,
};
use crate::localization::{Text, language};
use gpui::{App, Context, Div, Entity, Role, Window, prelude::*, px};
use windshare_ui::{Button, ButtonStyle, Icon, Input, Theme, Tone, column, row};

pub(super) fn account_feedback_banner(
    feedback: Option<&Result<Text, AccountFormError>>,
    page: AccountPage,
    theme: Theme,
    cx: &App,
) -> Option<gpui::Stateful<Div>> {
    let result = feedback?;
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
                "account_storage_error" => Some(Text::AccountStorageError),
                _ if matches!(
                    page,
                    AccountPage::Registration | AccountPage::Email(EmailPurpose::Registration, _)
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

impl Render for AccountDialog {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let theme = Theme::current(window, cx);
        let mut panel = match self.page() {
            AccountPage::Registration => self.registration_body(theme, cx),
            AccountPage::Overview => self.overview_body(theme, cx),
            _ => self.editor_body(theme, cx),
        };
        if let Some(banner) =
            account_feedback_banner(self.feedback.as_ref(), self.page(), theme, cx)
        {
            panel = panel.child(banner);
        }
        panel
    }
}

impl AccountDialog {
    fn registration_body(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let Some(view) = self.view(cx) else {
            return column();
        };
        let locale = language(cx);
        let create = Button::new(
            "create-account",
            locale.text(if matches!(self.feedback, Some(Err(_))) {
                Text::RetryConnection
            } else {
                Text::CreateAccount
            }),
        )
        .icon(Icon::Arrow)
        .style(ButtonStyle::Primary)
        .disabled(!self.can_register(cx))
        .on_press(cx.listener(|this, (), _, cx| this.register(cx)));
        let login = Button::new("registration-login", locale.text(Text::LoginExisting))
            .icon(Icon::Lock)
            .style(ButtonStyle::Quiet)
            .disabled(self.busy(cx))
            .on_press(cx.listener(|this, (), _, cx| this.open_page(AccountPage::Login, cx)));
        registration_panel_view(theme, view, self.registering(cx), true, create, login, cx)
    }

    fn overview_body(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let Some(view) = self.view(cx) else {
            return column();
        };
        let locale = language(cx);
        let locked = view.nickname_locked_until().is_some();
        let nickname = Button::new(
            "account-nickname",
            locale.text(AccountPage::Nickname(()).title(view)),
        )
        .icon(Icon::Pencil)
        .style(ButtonStyle::Secondary)
        .compact()
        .disabled(self.busy(cx) || locked)
        .on_press(cx.listener(|this, (), _, cx| {
            this.open_page(AccountPage::Nickname(()), cx);
        }));
        let copy = Button::new(
            "copy-account-uid",
            locale.text(if self.uid_copied {
                Text::Copied
            } else {
                Text::CopyUid
            }),
        )
        .icon(if self.uid_copied {
            Icon::Check
        } else {
            Icon::Copy
        })
        .style(if self.uid_copied {
            ButtonStyle::Tinted(Tone::Mint)
        } else {
            ButtonStyle::Secondary
        })
        .compact()
        .on_press(cx.listener(|this, (), _, cx| this.copy_uid(cx)));
        account_identity_view(theme, view, nickname, copy, cx)
    }

    fn editor_body(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let Some(view) = self.view(cx) else {
            return column();
        };
        let locale = language(cx);
        let field = |input: &Entity<Input>, label| {
            column()
                .gap(px(7.))
                .child(theme.caption(locale.text(label)))
                .child(input.clone())
        };
        let mut panel = column().gap(px(16.));
        let submit = match self.page() {
            AccountPage::Login | AccountPage::Credentials(()) => {
                let login = self.page() == AccountPage::Login;
                panel = panel
                    .child(theme.caption(locale.text(if login {
                        Text::AccountNewShares
                    } else {
                        Text::AccountPasswordDetail
                    })))
                    .child(field(
                        &self.username,
                        if login {
                            Text::AccountUsername
                        } else {
                            Text::CredentialUsername
                        },
                    ))
                    .child(field(
                        &self.password,
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
                    .child(field(&self.nickname, Text::NicknameField))
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
                    .child(field(&self.code, Text::AccountCode));
                Text::AccountVerify
            }
            AccountPage::Email(purpose, EmailStep::Address) => {
                panel = panel
                    .when(purpose == EmailPurpose::Registration, |panel| {
                        panel.child(theme.caption(locale.text(Text::AccountEmailNeeded)))
                    })
                    .child(field(&self.email, Text::AccountEmailLabel))
                    .child(theme.caption(locale.text(Text::SupportedEmailProviders)));
                Text::AccountSendCode
            }
            AccountPage::Overview | AccountPage::Registration => return panel,
        };
        panel.child(self.editor_actions(submit, cx))
    }

    fn editor_actions(&self, submit: Text, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let submit_icon = match submit {
            Text::SaveSettings | Text::AccountVerify => Icon::Check,
            _ => Icon::Arrow,
        };
        let busy = self.busy(cx);
        let can_submit = self.can_submit(cx);
        let mut actions = row().gap(px(8.)).flex_wrap().child(
            Button::new(
                "account-submit",
                locale.text(if busy { Text::AccountWorking } else { submit }),
            )
            .icon(submit_icon)
            .style(ButtonStyle::Primary)
            .disabled(!can_submit)
            .on_press(cx.listener(|this, (), _, cx| this.submit(cx))),
        );
        if let AccountPage::Email(purpose, EmailStep::Code) = self.page() {
            actions = actions.child(
                Button::new("new-email-code", locale.text(Text::AccountRequestAnother))
                    .icon(Icon::Refresh)
                    .style(ButtonStyle::Quiet)
                    .disabled(busy)
                    .on_press(cx.listener(move |this, (), _, cx| {
                        this.open_page(AccountPage::Email(purpose, EmailStep::Address), cx);
                    })),
            );
        }
        actions.children(self.alternative_button(cx))
    }

    fn alternative_button(&self, cx: &Context<Self>) -> Option<Button> {
        enum Alternative {
            Close,
            Open(AccountPage),
        }
        let view = self.view(cx)?;
        let (label, target, icon) = match self.page() {
            AccountPage::Email(purpose, EmailStep::Address) if purpose.has_code(view) => (
                Text::EnterExistingCode,
                Alternative::Open(AccountPage::Email(purpose, EmailStep::Code)),
                Icon::Shield,
            ),
            AccountPage::Login
            | AccountPage::Credentials(())
            | AccountPage::Nickname(())
            | AccountPage::Email(EmailPurpose::Profile(()), _)
                if self.origin == AccountOrigin::Settings =>
            {
                (Text::Cancel, Alternative::Close, Icon::Close)
            }
            AccountPage::Login
            | AccountPage::Credentials(())
            | AccountPage::Nickname(())
            | AccountPage::Email(EmailPurpose::Profile(()), _)
                if !view.status.account.is_empty() =>
            {
                (
                    Text::BackToAccount,
                    Alternative::Open(AccountPage::Overview),
                    Icon::ArrowLeft,
                )
            }
            AccountPage::Email(EmailPurpose::Registration, _) => (
                Text::LoginExisting,
                Alternative::Open(AccountPage::Login),
                Icon::Arrow,
            ),
            AccountPage::Login if view.status.account.is_empty() => (
                Text::SetUpAccount,
                Alternative::Open(AccountPage::entry(view).0),
                Icon::User,
            ),
            _ => return None,
        };
        let button = Button::new("account-alternative", language(cx).text(label))
            .icon(icon)
            .style(ButtonStyle::Quiet)
            .disabled(self.busy(cx));
        Some(match target {
            Alternative::Close => {
                button.on_press(cx.listener(|_, (), _, cx| cx.emit(AccountDialogEvent::Close)))
            }
            Alternative::Open(page) => {
                button.on_press(cx.listener(move |this, (), _, cx| this.open_page(page, cx)))
            }
        })
    }
}
