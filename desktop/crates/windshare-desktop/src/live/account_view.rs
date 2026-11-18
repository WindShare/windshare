use super::{
    Runtime,
    account::{AccountAction, FormMode},
};
use crate::{
    desktop::Desktop,
    localization::{Language, Text, language},
    model::{Action, ShareState},
};
use gpui::{Div, Entity, prelude::*, px};
use windshare_host::{AccountState, AccountView, Allowance};
use windshare_ui::{ButtonStyle, Icon, Input, Theme, Tone, column, row};

fn status(view: &AccountView, locale: Language) -> String {
    let value = &view.status;
    let text = match value.state {
        AccountState::PendingSetup if !value.active => Text::AccountDormant,
        AccountState::PendingSetup => Text::AccountSetup,
        AccountState::ProofOfWork => {
            return format!(
                "{} · {}s · {}M",
                locale.text(Text::AccountProof),
                value.proof_elapsed / 1_000_000_000,
                value.hashes / 1_000_000
            );
        }
        AccountState::EmailRequired => Text::AccountEmail,
        AccountState::Refreshing => Text::AccountRefreshing,
        AccountState::TemporarilyUnavailable => Text::AccountUnavailable,
        AccountState::Denied => Text::AccountDenied,
        AccountState::Ready => match value.allowance {
            Allowance::Spent => Text::AccountSpent,
            Allowance::Stale => Text::AccountStale,
            Allowance::Available => {
                return format!(
                    "{} · {} MiB",
                    locale.text(Text::AccountRemaining),
                    value.remaining_bytes / (1024 * 1024)
                );
            }
        },
    };
    locale.text(text).into()
}
impl Desktop {
    fn account_view(&self) -> Option<&AccountView> {
        if let Runtime::Native(session) = &self.runtime {
            session.model.account.as_ref()
        } else {
            None
        }
    }
    pub fn account_card(&self, theme: Theme, cx: &Context<Self>) -> Option<Div> {
        let view = self.account_view()?;
        let locale = language(cx);
        Some(
            theme.card().p(px(28.)).gap(px(16.)).child(
                row()
                    .justify_between()
                    .gap(px(16.))
                    .child(
                        column()
                            .gap(px(8.))
                            .child(theme.heading(locale.text(Text::Account), 22.))
                            .child(theme.caption(status(view, locale))),
                    )
                    .child(Self::button(
                        "manage-account",
                        locale.text(Text::ManageAccount),
                        Icon::Shield,
                        ButtonStyle::Secondary,
                        Action::OpenAccount,
                        cx,
                    )),
            ),
        )
    }
    pub fn account_notice(&self, theme: Theme, cx: &Context<Self>) -> Option<Div> {
        let view = self.account_view()?;
        let active_share = self.shares.iter().any(|share| !share.finished());
        if !active_share
            || !view.status.active
            || view.status.state == AccountState::Ready
                && view.status.allowance == Allowance::Available
        {
            return None;
        }
        // Available anonymous and independent paths remain usable; this notice
        // describes official account access without changing the share state.
        let waiting = self
            .shares
            .iter()
            .any(|share| matches!(share.state, ShareState::Preparing));
        if !waiting
            && matches!(
                view.status.state,
                AccountState::PendingSetup | AccountState::ProofOfWork
            )
        {
            return None;
        }
        Some(
            row()
                .gap(px(12.))
                .justify_between()
                .child(theme.caption(status(view, language(cx))))
                .child(Self::button(
                    "account-attention",
                    language(cx).text(Text::ManageAccount),
                    Icon::Shield,
                    ButtonStyle::Quiet,
                    Action::OpenAccount,
                    cx,
                )),
        )
    }
    pub fn account_panel(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let Some(view) = self.account_view() else {
            return column();
        };
        let locale = language(cx);
        let form = &self.account_form;
        let mut panel = column()
            .gap(px(18.))
            .child(theme.label(status(view, locale), 16.));
        if !view.status.account.is_empty() {
            panel = panel.child(theme.caption(format!(
                "{} · {}",
                locale.text(Text::AccountReset),
                view.reset_local
            )));
        }
        if let Some(error) = &view.error {
            panel = panel.child(
                theme
                    .caption(error.message.clone())
                    .text_color(Tone::Rose.text(theme)),
            );
        }
        panel = panel.child(self.account_modes(theme, cx));
        let field = |input: &Entity<Input>, text| {
            column()
                .gap(px(8.))
                .child(theme.caption(locale.text(text)))
                .child(input.clone())
        };
        if form.mode == FormMode::Email {
            panel = panel
                .child(field(&form.email, Text::AccountEmailLabel))
                .child(
                    Self::button(
                        "account-send-email",
                        locale.text(Text::AccountSendCode),
                        Icon::Link,
                        ButtonStyle::Secondary,
                        Action::Account(AccountAction::SendEmail),
                        cx,
                    )
                    .disabled(view.busy || form.email.read(cx).value().trim().is_empty()),
                )
                .child(field(&form.code, Text::AccountCode));
        } else {
            panel = panel
                .child(field(&form.username, Text::AccountUsername))
                .child(field(&form.password, Text::AccountPasswordLabel));
        }
        let submit = match form.mode {
            FormMode::Login => Text::AccountLogin,
            FormMode::Password => Text::AccountPassword,
            FormMode::Email => Text::AccountVerify,
        };
        let empty = if form.mode == FormMode::Email {
            form.code.read(cx).value().trim().is_empty()
        } else {
            form.username.read(cx).value().trim().is_empty()
                || form.password.read(cx).value().is_empty()
        };
        panel
            .child(
                Self::button(
                    "account-submit",
                    locale.text(if view.busy {
                        Text::AccountWorking
                    } else {
                        submit
                    }),
                    Icon::Check,
                    ButtonStyle::Primary,
                    Action::Account(AccountAction::Submit),
                    cx,
                )
                .disabled(view.busy || empty),
            )
            .child(theme.caption(locale.text(Text::AccountNewShares)))
    }
    fn account_modes(&self, _theme: Theme, cx: &Context<Self>) -> Div {
        let Some(view) = self.account_view() else {
            return row();
        };
        let locale = language(cx);
        let mut actions = vec![(Text::AccountLogin, AccountAction::ChooseLogin)];
        if !view.status.account.is_empty() {
            actions.push((Text::AccountPassword, AccountAction::ChoosePassword));
        }
        if view.status.state == AccountState::EmailRequired {
            actions.push((Text::AccountEmail, AccountAction::ChooseEmail));
        }
        if matches!(
            view.status.state,
            AccountState::TemporarilyUnavailable | AccountState::Denied
        ) {
            actions.push((Text::RetryConnection, AccountAction::Retry));
        }
        row()
            .gap(px(8.))
            .flex_wrap()
            .children(
                actions
                    .into_iter()
                    .enumerate()
                    .map(|(index, (text, action))| {
                        Self::button(
                            ("account-mode", index),
                            locale.text(text),
                            Icon::Shield,
                            ButtonStyle::Quiet,
                            Action::Account(action),
                            cx,
                        )
                        .disabled(view.busy)
                    }),
            )
    }
}
