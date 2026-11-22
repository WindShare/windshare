use super::{
    AccountPage, EmailPurpose, account_status_summary, can_register_account,
    dialog_view::account_feedback_banner, registration::registration_panel_view,
};
use crate::{
    desktop::Desktop,
    live::byte_size,
    localization::{Language, Text, language},
    model::{Page, SettingsSection},
    network::setting_row,
};
use gpui::{App, Context, Div, FontWeight, prelude::*, px};
use windshare_host::{AccountState, AccountView, Allowance};
use windshare_ui::{Button, ButtonStyle, FONT, Icon, Theme, Tone, column, row};

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
        AccountState::StorageError => (Text::AccountStatusStorageError, Tone::Rose),
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

pub(super) fn account_identity_view(
    theme: Theme,
    view: &AccountView,
    nickname_button: Button,
    copy_uid_button: Button,
    cx: &App,
) -> Div {
    let locale = language(cx);
    let locked_until = view.nickname_locked_until();
    let (badge_label, badge_tone) = status_badge(view);
    let avatar = avatar_initial(view).map_or_else(
        || theme.icon_tile(Icon::User, Tone::Mint, AVATAR_SIZE),
        |initial| theme.avatar(&initial, Tone::Mint, AVATAR_SIZE),
    );
    let show_handle = view.status.profile.nickname.is_some() && !view.status.username.is_empty();
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
                    header.child(nickname_button)
                }),
        )
        .child(account_uid_strip(
            theme,
            &view.status.account,
            copy_uid_button,
            cx,
        ))
}

fn account_uid_strip(theme: Theme, account_id: &str, copy_button: Button, cx: &App) -> Div {
    let locale = language(cx);
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
        .child(copy_button.min_width(px(UID_COPY_MIN_WIDTH)))
}

impl Desktop {
    pub(crate) fn account_attention_tone(&self, cx: &gpui::App) -> Option<Tone> {
        let view = self.account_view(cx)?;
        match view.status.state {
            AccountState::Ready if view.status.allowance == Allowance::Available => None,
            AccountState::Ready if view.status.allowance == Allowance::Spent => Some(Tone::Peach),
            AccountState::EmailRequired => Some(Tone::Peach),
            AccountState::TemporarilyUnavailable
            | AccountState::StorageError
            | AccountState::Denied => Some(Tone::Rose),
            AccountState::Registering { .. } => Some(Tone::Mint),
            _ => None,
        }
    }

    pub fn account_settings_page(&self, theme: Theme, cx: &Context<Self>) -> Div {
        let Some(view) = self.account_view(cx) else {
            return column();
        };
        if view.status.state == AccountState::StorageError {
            return Self::account_storage_error_page(theme, view, cx);
        }
        if view.status.account.is_empty() {
            return self.account_onboarding_page(theme, view, cx);
        }
        let feedback = self.account_inline_feedback(cx);
        column()
            .gap(px(SECTION_GAP))
            .child(self.account_identity_card(theme, view, cx))
            .child(self.account_allowance_card(theme, view, cx))
            .child(self.account_security_card(theme, view, cx))
            .when_some(
                account_feedback_banner(feedback.as_ref(), AccountPage::Overview, theme, cx),
                gpui::ParentElement::child,
            )
    }

    fn account_modal_button(
        id: &'static str,
        label: Text,
        symbol: Icon,
        style: ButtonStyle,
        page: AccountPage,
        cx: &Context<Self>,
    ) -> Button {
        Button::new(id, language(cx).text(label))
            .icon(symbol)
            .style(style)
            .on_press(cx.listener(move |this, (), window, cx| {
                this.open_account_modal(page, window, cx);
                cx.notify();
            }))
    }

    fn account_storage_error_page(theme: Theme, view: &AccountView, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let (badge_label, badge_tone) = status_badge(view);
        let retry = Self::button(
            "reload-account-selection",
            locale.text(Text::RetryConnection),
            Icon::Refresh,
            ButtonStyle::Primary,
            crate::model::Action::ReloadAccountSelection,
            cx,
        );
        let mut details = column()
            .flex_1()
            .min_w_0()
            .gap(px(5.))
            .child(
                row()
                    .flex_wrap()
                    .gap(px(10.))
                    .child(theme.heading(locale.text(Text::Account), 18.))
                    .child(theme.badge(locale.text(badge_label), badge_tone)),
            )
            .child(
                theme
                    .caption(locale.text(Text::AccountStorageError))
                    .text_color(Tone::Rose.text(theme)),
            );
        if !view.status.reason.is_empty() {
            details = details.child(theme.caption(view.status.reason.clone()));
        }
        column().gap(px(SECTION_GAP)).child(
            theme
                .card()
                .px(px(CARD_PADDING_X))
                .py(px(CARD_PADDING_Y))
                .gap(px(18.))
                .child(
                    row()
                        .gap(px(14.))
                        .child(theme.icon_tile(Icon::Shield, Tone::Rose, ONBOARDING_ICON_SIZE))
                        .child(details),
                )
                .child(row().child(retry)),
        )
    }

    fn account_onboarding_page(&self, theme: Theme, view: &AccountView, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let busy = self.account_busy(cx);
        let feedback = self.account_inline_feedback(cx);
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
                            Self::account_modal_button(
                                "verify-registration-email",
                                Text::AccountEmail,
                                Icon::Shield,
                                ButtonStyle::Primary,
                                email_page,
                                cx,
                            )
                            .disabled(busy),
                        )
                        .child(
                            Self::account_modal_button(
                                "onboarding-login",
                                Text::LoginExisting,
                                Icon::Lock,
                                ButtonStyle::Quiet,
                                AccountPage::Login,
                                cx,
                            )
                            .disabled(busy),
                        ),
                )
        } else {
            let visible = self.page == Page::Settings
                && self.settings_section == SettingsSection::Account
                && self.overlays.is_empty();
            let create = Button::new(
                "create-account",
                locale.text(if matches!(feedback, Some(Err(_))) {
                    Text::RetryConnection
                } else {
                    Text::CreateAccount
                }),
            )
            .icon(Icon::Arrow)
            .style(ButtonStyle::Primary)
            .disabled(!can_register_account(view, busy))
            .on_press(cx.listener(|this, (), _, cx| this.register_account(cx)));
            let login = Self::account_modal_button(
                "registration-login",
                Text::LoginExisting,
                Icon::Lock,
                ButtonStyle::Quiet,
                AccountPage::Login,
                cx,
            );
            registration_panel_view(theme, view, busy, visible, create, login, cx)
        };
        column().gap(px(SECTION_GAP)).child(
            theme
                .card()
                .px(px(CARD_PADDING_X))
                .py(px(CARD_PADDING_Y))
                .gap(px(18.))
                .child(body)
                .when_some(
                    account_feedback_banner(
                        feedback.as_ref(),
                        AccountPage::Registration,
                        theme,
                        cx,
                    ),
                    gpui::ParentElement::child,
                ),
        )
    }

    fn account_identity_card(&self, theme: Theme, view: &AccountView, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let busy = self.account_busy(cx);
        let locked = view.nickname_locked_until().is_some();
        let nickname = Self::account_modal_button(
            "account-nickname",
            AccountPage::Nickname(()).title(view),
            Icon::Pencil,
            ButtonStyle::Secondary,
            AccountPage::Nickname(()),
            cx,
        )
        .compact()
        .disabled(busy || locked);
        let uid_copied = self.copy_feedback.is_account_uid();
        let copy = Button::new(
            "copy-account-uid",
            locale.text(if uid_copied {
                Text::Copied
            } else {
                Text::CopyUid
            }),
        )
        .icon(if uid_copied { Icon::Check } else { Icon::Copy })
        .style(if uid_copied {
            ButtonStyle::Tinted(Tone::Mint)
        } else {
            ButtonStyle::Secondary
        })
        .compact()
        .on_press(cx.listener(|this, (), _, cx| {
            this.copy_account_uid(cx);
            cx.notify();
        }));
        theme
            .card()
            .px(px(CARD_PADDING_X))
            .py(px(CARD_PADDING_Y))
            .child(account_identity_view(theme, view, nickname, copy, cx))
    }

    fn account_allowance_card(&self, theme: Theme, view: &AccountView, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let tile_tone = match view.status.state {
            AccountState::Ready if view.status.allowance == Allowance::Available => Tone::Mint,
            AccountState::Ready if view.status.allowance == Allowance::Spent => Tone::Peach,
            AccountState::EmailRequired => Tone::Peach,
            AccountState::TemporarilyUnavailable
            | AccountState::StorageError
            | AccountState::Denied => Tone::Rose,
            _ => Tone::Neutral,
        };
        let reset = view
            .allowance_reset_local()
            .map(|at| format!("{} · {at}", locale.text(Text::AccountReset)));
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
        let busy = self.account_busy(cx);
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
                    Self::account_modal_button(
                        "verify-account",
                        Text::AccountEmail,
                        Icon::Shield,
                        ButtonStyle::Primary,
                        EmailPurpose::Registration.entry(view),
                        cx,
                    )
                    .compact()
                    .disabled(busy),
                ),
            _ => {
                let text_tone = if matches!(
                    view.status.state,
                    AccountState::TemporarilyUnavailable
                        | AccountState::StorageError
                        | AccountState::Denied
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
                            .caption(account_status_summary(view, locale))
                            .text_color(text_tone),
                    )
                    .child(
                        Button::new("retry-account", locale.text(Text::RetryConnection))
                            .icon(Icon::Refresh)
                            .style(ButtonStyle::Secondary)
                            .compact()
                            .disabled(busy)
                            .on_press(cx.listener(|this, (), _, cx| this.refresh_account(cx))),
                    )
            }
        }
    }

    fn account_security_card(&self, theme: Theme, view: &AccountView, cx: &Context<Self>) -> Div {
        let locale = language(cx);
        let busy = self.account_busy(cx);
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
                        Self::account_modal_button(
                            "set-account-login",
                            AccountPage::Credentials(()).title(view),
                            Icon::Lock,
                            ButtonStyle::Secondary,
                            AccountPage::Credentials(()),
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
            Self::account_modal_button(
                "switch-account",
                switch_label,
                Icon::Arrow,
                ButtonStyle::Quiet,
                AccountPage::Login,
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
                Self::account_modal_button(
                    "account-profile-email",
                    email_button_label,
                    Icon::Mail,
                    ButtonStyle::Secondary,
                    email_page,
                    cx,
                )
                .compact()
                .disabled(busy),
            )
    }
}
