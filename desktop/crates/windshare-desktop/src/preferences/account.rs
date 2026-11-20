use crate::{
    desktop::Desktop,
    live::{AccountAction, AccountPage, EmailPurpose, byte_size, setting_row},
    localization::{Language, Text, language},
    model::Action,
};
use gpui::{Div, FontWeight, prelude::*, px};
use windshare_host::{AccountState, AccountView, Allowance};
use windshare_ui::{ButtonStyle, FONT, Icon, Theme, Tone, column, row};

const AVATAR_SIZE: f32 = 46.;
const CARD_ICON_SIZE: f32 = 36.;
const ONBOARDING_ICON_SIZE: f32 = 44.;
const CARD_PADDING_X: f32 = 24.;
const CARD_PADDING_Y: f32 = 20.;
const SECTION_GAP: f32 = 16.;
const UID_COPY_MIN_WIDTH: f32 = 88.;

const fn status_badge(view: &AccountView) -> (Text, Tone) {
    match view.status.state {
        AccountState::Ready => match view.status.allowance {
            Allowance::Available => (Text::AccountStatusReady, Tone::Mint),
            Allowance::Spent => (Text::AccountStatusSpent, Tone::Peach),
            Allowance::Stale => (Text::AccountStatusUpdating, Tone::Neutral),
        },
        AccountState::EmailRequired => (Text::AccountStatusEmailRequired, Tone::Peach),
        AccountState::RefreshRequired
        | AccountState::Refreshing
        | AccountState::Registering { .. } => (Text::AccountStatusUpdating, Tone::Neutral),
        AccountState::TemporarilyUnavailable => (Text::AccountStatusUnavailable, Tone::Rose),
        AccountState::Denied => (Text::AccountStatusDenied, Tone::Rose),
        AccountState::PendingSetup | AccountState::RegistrationRequired => {
            (Text::SetUpAccount, Tone::Neutral)
        }
    }
}

fn display_title(view: &AccountView, locale: Language) -> String {
    if let Some(nickname) = &view.status.profile.nickname {
        return nickname.value.clone();
    }
    if !view.status.username.is_empty() {
        return format!("@{}", view.status.username);
    }
    locale.text(Text::DeviceAccount).to_owned()
}

fn avatar_initial(view: &AccountView) -> Option<String> {
    let source = view
        .status
        .profile
        .nickname
        .as_ref()
        .map(|nickname| nickname.value.as_str())
        .filter(|value| !value.is_empty())
        .or_else(|| (!view.status.username.is_empty()).then_some(view.status.username.as_str()))?;
    let first = source.chars().next()?;
    Some(first.to_uppercase().collect())
}

impl Desktop {
    pub(crate) fn account_attention_tone(&self) -> Option<Tone> {
        let view = self.account_view()?;
        match view.status.state {
            AccountState::Ready if view.status.allowance == Allowance::Available => None,
            AccountState::Ready if view.status.allowance == Allowance::Spent => Some(Tone::Peach),
            AccountState::EmailRequired => Some(Tone::Peach),
            AccountState::TemporarilyUnavailable | AccountState::Denied => Some(Tone::Rose),
            AccountState::Registering { .. } => Some(Tone::Mint),
            _ => None,
        }
    }

    pub fn account_settings_page(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let Some(view) = self.account_view() else {
            return column();
        };
        if view.status.account.is_empty() {
            return self.account_onboarding_page(theme, view, cx);
        }
        column()
            .gap(px(SECTION_GAP))
            .child(self.account_identity_card(theme, view, cx))
            .child(self.account_allowance_card(theme, view, cx))
            .child(self.account_security_card(theme, view, cx))
            .when_some(self.account_feedback_banner(theme, cx), |col, banner| {
                col.child(banner)
            })
    }

    fn account_onboarding_page(&self, theme: Theme, view: &AccountView, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let busy = self.account_form.busy(view);
        let body = if view.status.state == AccountState::EmailRequired {
            let email_page = EmailPurpose::Registration.entry(view);
            column()
                .gap(px(18.))
                .child(
                    row()
                        .gap(px(14.))
                        .child(theme.icon_tile(Icon::Mail, Tone::Peach, ONBOARDING_ICON_SIZE))
                        .child(
                            column()
                                .flex_1()
                                .min_w_0()
                                .gap(px(4.))
                                .child(theme.heading(locale.text(Text::AccountEmail), 18.))
                                .child(theme.caption(locale.text(Text::AccountEmailNeeded))),
                        ),
                )
                .child(
                    row()
                        .flex_wrap()
                        .gap(px(8.))
                        .child(
                            Self::button(
                                "verify-registration-email",
                                locale.text(Text::AccountEmail),
                                Icon::Shield,
                                ButtonStyle::Primary,
                                Action::OpenAccountModal(email_page),
                                cx,
                            )
                            .disabled(busy),
                        )
                        .child(
                            Self::button(
                                "onboarding-login",
                                locale.text(Text::LoginExisting),
                                Icon::Lock,
                                ButtonStyle::Quiet,
                                Action::OpenAccountModal(AccountPage::Login),
                                cx,
                            )
                            .disabled(busy),
                        ),
                )
        } else {
            self.registration_panel(theme, view, cx)
        };
        column().gap(px(SECTION_GAP)).child(
            theme
                .card()
                .px(px(CARD_PADDING_X))
                .py(px(CARD_PADDING_Y))
                .gap(px(18.))
                .child(body)
                .when_some(self.account_feedback_banner(theme, cx), |card, banner| {
                    card.child(banner)
                }),
        )
    }

    pub(crate) fn account_identity_content(
        &self,
        theme: Theme,
        view: &AccountView,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let busy = self.account_form.busy(view);
        let locked_until = view.nickname_locked_until();
        let (badge_label, badge_tone) = status_badge(view);
        let avatar = avatar_initial(view).map_or_else(
            || theme.icon_tile(Icon::User, Tone::Mint, AVATAR_SIZE),
            |initial| theme.avatar(&initial, Tone::Mint, AVATAR_SIZE),
        );
        let show_handle =
            view.status.profile.nickname.is_some() && !view.status.username.is_empty();
        let meta = match (show_handle, locked_until) {
            (true, Some(at)) => Some(format!(
                "@{} · {}",
                view.status.username,
                locale.nickname_next_change(at)
            )),
            (true, None) => Some(format!("@{}", view.status.username)),
            (false, Some(at)) => Some(locale.nickname_next_change(at)),
            (false, None) => None,
        };
        column()
            .gap(px(16.))
            .child(
                row()
                    .gap(px(14.))
                    .child(avatar)
                    .child(
                        column()
                            .flex_1()
                            .min_w_0()
                            .gap(px(4.))
                            .child(
                                row()
                                    .flex_wrap()
                                    .gap(px(10.))
                                    .child(theme.heading(display_title(view, locale), 20.))
                                    .child(theme.badge(locale.text(badge_label), badge_tone)),
                            )
                            .when_some(meta, |col, text| col.child(theme.caption(text))),
                    )
                    .when(view.can_edit_profile(), |header| {
                        header.child(
                            Self::button(
                                "account-nickname",
                                locale.text(AccountPage::Nickname(()).title(view)),
                                Icon::Pencil,
                                ButtonStyle::Secondary,
                                Action::OpenAccountModal(AccountPage::Nickname(())),
                                cx,
                            )
                            .compact()
                            .disabled(busy || locked_until.is_some()),
                        )
                    }),
            )
            .child(self.account_uid_strip(theme, &view.status.account, cx))
    }

    fn account_identity_card(&self, theme: Theme, view: &AccountView, cx: &Context<Self>) -> Div {
        theme
            .card()
            .px(px(CARD_PADDING_X))
            .py(px(CARD_PADDING_Y))
            .child(self.account_identity_content(theme, view, cx))
    }

    fn account_uid_strip(&self, theme: Theme, account_id: &str, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let uid_copied = self.copy_feedback.is_account_uid();
        row()
            .justify_between()
            .gap(px(12.))
            .px(px(14.))
            .py(px(8.))
            .rounded(px(12.))
            .bg(theme.inset())
            .border_1()
            .border_color(theme.border().opacity(0.6))
            .child(
                row()
                    .flex_1()
                    .min_w_0()
                    .gap(px(10.))
                    .child(
                        theme
                            .label(locale.text(Text::AccountUid), 12.)
                            .font_family(FONT)
                            .font_weight(FontWeight::MEDIUM)
                            .text_color(theme.muted_text()),
                    )
                    .child(
                        theme
                            .label(account_id.to_owned(), 14.)
                            .font_family(FONT)
                            .flex_1()
                            .min_w_0()
                            .truncate(),
                    ),
            )
            .child(
                Self::button(
                    "copy-account-uid",
                    locale.text(if uid_copied {
                        Text::Copied
                    } else {
                        Text::CopyUid
                    }),
                    if uid_copied { Icon::Check } else { Icon::Copy },
                    if uid_copied {
                        ButtonStyle::Tinted(Tone::Mint)
                    } else {
                        ButtonStyle::Secondary
                    },
                    Action::CopyAccountUid,
                    cx,
                )
                .compact()
                .min_width(px(UID_COPY_MIN_WIDTH)),
            )
    }

    fn account_allowance_card(&self, theme: Theme, view: &AccountView, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let tile_tone = match view.status.state {
            AccountState::Ready if view.status.allowance == Allowance::Available => Tone::Mint,
            AccountState::Ready if view.status.allowance == Allowance::Spent => Tone::Peach,
            AccountState::EmailRequired => Tone::Peach,
            AccountState::TemporarilyUnavailable | AccountState::Denied => Tone::Rose,
            _ => Tone::Neutral,
        };
        let reset = (view.status.state == AccountState::Ready && !view.reset_local.is_empty())
            .then(|| format!("{} · {}", locale.text(Text::AccountReset), view.reset_local));
        theme
            .card()
            .px(px(CARD_PADDING_X))
            .py(px(CARD_PADDING_Y))
            .gap(px(14.))
            .child(
                row()
                    .justify_between()
                    .flex_wrap()
                    .gap(px(12.))
                    .child(
                        row()
                            .gap(px(12.))
                            .child(theme.icon_tile(Icon::Wind, tile_tone, CARD_ICON_SIZE))
                            .child(
                                theme.heading(locale.text(Text::AccountRegistrationBenefit), 18.),
                            ),
                    )
                    .when_some(reset, |header, text| header.child(theme.caption(text))),
            )
            .child(self.account_allowance_body(theme, view, cx))
    }

    fn account_allowance_body(&self, theme: Theme, view: &AccountView, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let busy = self.account_form.busy(view);
        match view.status.state {
            AccountState::Ready if view.status.allowance == Allowance::Available => row()
                .flex_wrap()
                .gap(px(10.))
                .items_baseline()
                .child(
                    theme
                        .heading(byte_size(view.status.remaining_bytes), 28.)
                        .font_family(FONT)
                        .text_color(theme.accent_text()),
                )
                .child(theme.caption(locale.text(Text::AccountRemainingShort))),
            AccountState::Ready if view.status.allowance == Allowance::Spent => column()
                .gap(px(6.))
                .child(
                    theme
                        .heading("0 B", 24.)
                        .font_family(FONT)
                        .text_color(Tone::Peach.text(theme)),
                )
                .child(theme.caption(locale.text(Text::AccountSpent))),
            AccountState::EmailRequired => row()
                .justify_between()
                .flex_wrap()
                .gap(px(12.))
                .child(
                    theme
                        .caption(locale.text(Text::AccountEmailNeeded))
                        .text_color(Tone::Peach.text(theme)),
                )
                .child(
                    Self::button(
                        "verify-account",
                        locale.text(Text::AccountEmail),
                        Icon::Shield,
                        ButtonStyle::Primary,
                        Action::OpenAccountModal(EmailPurpose::Registration.entry(view)),
                        cx,
                    )
                    .compact()
                    .disabled(busy),
                ),
            _ => {
                let text_tone = if matches!(
                    view.status.state,
                    AccountState::TemporarilyUnavailable | AccountState::Denied
                ) {
                    Tone::Rose.text(theme)
                } else {
                    theme.muted_text()
                };
                row()
                    .justify_between()
                    .flex_wrap()
                    .gap(px(12.))
                    .child(
                        theme
                            .caption(Self::account_status_summary(view, locale))
                            .text_color(text_tone),
                    )
                    .child(
                        Self::button(
                            "refresh-account",
                            locale.text(Text::RetryConnection),
                            Icon::Refresh,
                            ButtonStyle::Secondary,
                            Action::Account(AccountAction::Refresh),
                            cx,
                        )
                        .compact()
                        .disabled(busy),
                    )
            }
        }
    }

    fn account_security_card(&self, theme: Theme, view: &AccountView, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let busy = self.account_form.busy(view);
        let credentials_detail = if view.status.username.is_empty() {
            locale.text(Text::AccountPasswordDetail).to_owned()
        } else {
            format!("@{}", view.status.username)
        };
        let switch_label = if view.status.username.is_empty() {
            Text::LoginExisting
        } else {
            Text::SwitchAccount
        };
        let mut rows = column();
        if view.can_edit_profile() {
            rows = rows
                .child(Self::account_email_row(theme, view, busy, cx).py(px(13.)))
                .child(
                    setting_row(
                        theme,
                        locale.text(Text::AccountCredentials),
                        credentials_detail,
                        Self::button(
                            "set-account-login",
                            locale.text(AccountPage::Credentials(()).title(view)),
                            Icon::Lock,
                            ButtonStyle::Secondary,
                            Action::OpenAccountModal(AccountPage::Credentials(())),
                            cx,
                        )
                        .disabled(busy),
                    )
                    .py(px(13.))
                    .border_t_1()
                    .border_color(theme.border()),
                );
        }
        let switch_row = setting_row(
            theme,
            locale.text(switch_label),
            locale.text(Text::SwitchAccountDetail).to_owned(),
            Self::button(
                "switch-account",
                locale.text(switch_label),
                Icon::Arrow,
                ButtonStyle::Quiet,
                Action::OpenAccountModal(AccountPage::Login),
                cx,
            )
            .disabled(busy),
        )
        .pt(px(13.));
        rows = rows.child(if view.can_edit_profile() {
            switch_row.border_t_1().border_color(theme.border())
        } else {
            switch_row
        });
        theme
            .card()
            .px(px(CARD_PADDING_X))
            .py(px(CARD_PADDING_Y))
            .gap(px(6.))
            .child(
                row()
                    .gap(px(12.))
                    .pb(px(4.))
                    .child(theme.icon_tile(Icon::Shield, Tone::Blue, CARD_ICON_SIZE))
                    .child(theme.heading(locale.text(Text::AccountSecurity), 18.)),
            )
            .child(rows)
    }

    fn account_email_row(theme: Theme, view: &AccountView, busy: bool, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let email = &view.status.profile.email;
        let email_page = EmailPurpose::Profile(()).entry(view);
        let email_button_label = if view.status.pending_email.is_some() {
            Text::ContinueVerification
        } else {
            email_page.title(view)
        };
        row()
            .gap(px(18.))
            .child(
                column()
                    .flex_1()
                    .min_w_0()
                    .gap(px(4.))
                    .child(
                        row()
                            .flex_wrap()
                            .gap(px(8.))
                            .child(
                                theme
                                    .label(locale.text(Text::AccountEmailLabel), 15.)
                                    .font_weight(FontWeight::MEDIUM),
                            )
                            .when(view.status.pending_email.is_some(), |r| {
                                r.child(theme.badge(
                                    locale.text(Text::EmailPendingVerification),
                                    Tone::Peach,
                                ))
                            }),
                    )
                    .child(theme.caption(if email.is_empty() {
                        locale.text(Text::EmailNotSet).to_owned()
                    } else {
                        email.clone()
                    }))
                    .when_some(view.status.pending_email.as_ref(), |col, pending| {
                        col.child(
                            theme
                                .caption(pending.address.clone())
                                .text_color(Tone::Peach.text(theme)),
                        )
                    }),
            )
            .child(
                Self::button(
                    "account-profile-email",
                    locale.text(email_button_label),
                    Icon::Mail,
                    ButtonStyle::Secondary,
                    Action::OpenAccountModal(email_page),
                    cx,
                )
                .compact()
                .disabled(busy),
            )
    }
}
