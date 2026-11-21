use crate::{
    desktop::Desktop,
    localization::{Language, Text, language},
    model::{Action, Share},
};
use gpui::{Div, prelude::*, px};
use std::time::{Duration, SystemTime};
use windshare_host::{AccountState, BlockingReason, CreationBlock, RecoveryAction};
use windshare_ui::{ButtonStyle, Icon, Theme, column, row};

const BLOCKED_CARD_WIDTH: f32 = 560.;
const SECONDS_PER_MINUTE: u64 = 60;
const SECONDS_PER_HOUR: u64 = 60 * SECONDS_PER_MINUTE;
const SECONDS_PER_DAY: u64 = 24 * SECONDS_PER_HOUR;

pub fn retry_deadline(blocking: CreationBlock) -> Option<SystemTime> {
    blocking
        .retry_at_ms
        .and_then(|at| SystemTime::UNIX_EPOCH.checked_add(Duration::from_millis(at)))
}

impl Desktop {
    fn setup_blocked_share(&self, theme: Theme, share: &Share, cx: &Context<Self>) -> Option<Div> {
        let crate::model::ShareSource::Native { operation_id } = &share.source else {
            return None;
        };
        let progress = self.setup_return(theme, operation_id, cx)?;
        Some(
            column().w_full().items_center().py(px(24.)).child(
                theme
                    .card()
                    .w_full()
                    .max_w(px(BLOCKED_CARD_WIDTH))
                    .p(px(28.))
                    .gap(px(20.))
                    .child(theme.heading(share.name.clone(), 24.))
                    .child(progress)
                    .child(Self::button(
                        "cancel-share-awaiting-setup",
                        language(cx).text(Text::Cancel),
                        Icon::Close,
                        ButtonStyle::Quiet,
                        Action::StopShare(share.id),
                        cx,
                    )),
            ),
        )
    }

    pub fn authorization_share(&self, theme: Theme, share: &Share, cx: &Context<Self>) -> Div {
        if let Some(progress) = self.setup_blocked_share(theme, share, cx) {
            return progress;
        }
        let locale = language(cx);
        let official = matches!(
            share.authorization,
            Some(windshare_host::ShareAuthorization::Official)
        );
        let account = self.account_view().filter(|_| official);
        if let Some(view) = account.filter(|view| view.status.state == AccountState::StorageError) {
            return Self::account_storage_error_share(theme, share, &view.status.reason, cx);
        }
        let (label, action) = match (official, account) {
            (_, Some(view)) => (
                crate::account::AccountPage::entry(view).1,
                Action::ConfigureShare(share.id),
            ),
            (true, None) => (Text::ChooseRelays, Action::ChooseShareRelays(share.id)),
            (false, None) => (Text::ApproveDevice, Action::ConfigureShare(share.id)),
        };
        column().w_full().items_center().py(px(24.)).child(
            theme
                .card()
                .w_full()
                .max_w(px(BLOCKED_CARD_WIDTH))
                .p(px(28.))
                .gap(px(20.))
                .child(theme.heading(locale.text(Text::AuthorizationRequired), 24.))
                .child(theme.label(share.name.clone(), 16.))
                .when(official, |card| {
                    card.child(theme.caption(locale.text(Text::OfficialAuthorizationRequired)))
                })
                .child(
                    row()
                        .gap(px(10.))
                        .flex_wrap()
                        .child(Self::button(
                            "authorize-share",
                            locale.text(label),
                            Icon::Shield,
                            ButtonStyle::Primary,
                            action,
                            cx,
                        ))
                        .when(account.is_some(), |row| {
                            row.child(Self::button(
                                "other-relays",
                                locale.text(Text::ChooseRelays),
                                Icon::Sliders,
                                ButtonStyle::Secondary,
                                Action::ChooseShareRelays(share.id),
                                cx,
                            ))
                        })
                        .child(Self::button(
                            "cancel-authorization",
                            locale.text(Text::Cancel),
                            Icon::Close,
                            ButtonStyle::Quiet,
                            Action::StopShare(share.id),
                            cx,
                        )),
                ),
        )
    }

    fn account_storage_error_share(
        theme: Theme,
        share: &Share,
        reason: &str,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let mut card = theme
            .card()
            .w_full()
            .max_w(px(BLOCKED_CARD_WIDTH))
            .p(px(28.))
            .gap(px(20.))
            .child(theme.heading(locale.text(Text::SetupNeedsAttention), 24.))
            .child(theme.label(share.name.clone(), 16.))
            .child(theme.caption(locale.text(Text::OfficialAccountStorageError)));
        if !reason.is_empty() {
            card = card.child(theme.caption(reason.to_owned()));
        }
        column().w_full().items_center().py(px(24.)).child(
            card.child(
                row()
                    .gap(px(10.))
                    .flex_wrap()
                    .child(Self::button(
                        "other-relays",
                        locale.text(Text::ChooseRelays),
                        Icon::Sliders,
                        ButtonStyle::Primary,
                        Action::ChooseShareRelays(share.id),
                        cx,
                    ))
                    .child(Self::button(
                        "reload-account",
                        locale.text(Text::RetryConnection),
                        Icon::Refresh,
                        ButtonStyle::Secondary,
                        Action::ReloadAccountSelection,
                        cx,
                    ))
                    .child(Self::button(
                        "cancel-authorization",
                        locale.text(Text::Cancel),
                        Icon::Close,
                        ButtonStyle::Quiet,
                        Action::StopShare(share.id),
                        cx,
                    )),
            ),
        )
    }

    pub fn blocked_share(
        &self,
        theme: Theme,
        share: &Share,
        blocking: CreationBlock,
        cx: &Context<Self>,
    ) -> Div {
        if let Some(progress) = self.setup_blocked_share(theme, share, cx) {
            return progress;
        }
        let locale = language(cx);
        let message = match blocking.reason {
            BlockingReason::SettingsUnavailable => Text::SettingsNeedRepair,
            BlockingReason::ConfigurationRequired => Text::RelayRequired,
            BlockingReason::Unprovisioned => Text::NetworkNotProvisioned,
            BlockingReason::InvalidNetworkConfiguration => Text::NetworkBuildInvalid,
            BlockingReason::TrustStateUnreadable => Text::TrustStateUnreadable,
            BlockingReason::TrustStateInvalid => Text::TrustStateInvalid,
            BlockingReason::TrustStateUnwritable => Text::TrustStateUnwritable,
            BlockingReason::SelectedRelaysUnavailable => Text::SelectedRelaysUnavailable,
            BlockingReason::NoRelayCandidates => Text::NoRelayCandidates,
            BlockingReason::DirectoryUnavailable | BlockingReason::AliasUnavailable => {
                Text::DirectoryUnavailable
            }
        };
        let continuation = match blocking.recovery {
            RecoveryAction::Retry => Some((Text::RetryConnection, Action::ResumeShare(share.id))),
            RecoveryAction::RepairSettings => {
                Some((Text::UseSavedSettings, Action::UseSavedSettings(share.id)))
            }
            _ => None,
        };
        let cooling_down = retry_deadline(blocking).is_some_and(|at| at > SystemTime::now());
        let mut card = theme
            .card()
            .w_full()
            .max_w(px(BLOCKED_CARD_WIDTH))
            .p(px(28.))
            .gap(px(20.))
            .child(theme.heading(locale.text(share.status().0), 24.))
            .child(theme.label(share.name.clone(), 16.))
            .child(theme.caption(locale.text(message)));
        if cooling_down && let Some(at) = blocking.retry_at_ms {
            card = card.child(theme.caption(retry_time(locale, at)));
        }
        if blocking.recovery == RecoveryAction::RepairSettings
            && let Some(error) = &share.error
        {
            card = card.child(theme.caption(error.clone()));
        }
        let mut actions = row().gap(px(10.)).flex_wrap();
        if let Some((label, action)) = continuation {
            actions = actions.child(
                Self::button(
                    "retry-share",
                    locale.text(label),
                    Icon::Refresh,
                    ButtonStyle::Primary,
                    action,
                    cx,
                )
                .disabled(cooling_down || self.runtime.selection_disabled()),
            );
        }
        card = card.child(
            actions
                .child(Self::button(
                    "configure-relay",
                    locale.text(Text::ChooseRelays),
                    Icon::Settings,
                    ButtonStyle::Secondary,
                    Action::ConfigureShare(share.id),
                    cx,
                ))
                .child(Self::button(
                    "cancel-blocked-share",
                    locale.text(Text::Cancel),
                    Icon::Close,
                    ButtonStyle::Quiet,
                    Action::ResetShare,
                    cx,
                )),
        );
        column().w_full().items_center().py(px(24.)).child(card)
    }
}

fn retry_time(locale: Language, milliseconds: u64) -> String {
    let seconds = milliseconds.div_ceil(1000) % SECONDS_PER_DAY;
    let clock = format!(
        "{:02}:{:02}:{:02} UTC",
        seconds / SECONDS_PER_HOUR,
        seconds / SECONDS_PER_MINUTE % SECONDS_PER_MINUTE,
        seconds % SECONDS_PER_MINUTE
    );
    match locale {
        Language::English => format!("Retry available after {clock}"),
        Language::SimplifiedChinese => format!("可在 {clock} 后重试"),
    }
}
