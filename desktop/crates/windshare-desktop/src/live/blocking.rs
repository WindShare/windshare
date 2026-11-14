use crate::{
    desktop::Desktop,
    localization::{Language, Text, language},
    model::{Action, Share},
};
use gpui::{Div, prelude::*, px};
use std::time::{Duration, SystemTime};
use windshare_host::{BlockingReason, CreationBlock, RecoveryAction};
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
    pub fn blocked_share(
        &self,
        theme: Theme,
        share: &Share,
        blocking: CreationBlock,
        cx: &Context<Self>,
    ) -> Div {
        let locale = language(cx);
        let message = match blocking.reason {
            BlockingReason::SettingsUnavailable => Text::SettingsNeedRepair,
            BlockingReason::ConfigurationRequired => Text::RelayRequired,
            BlockingReason::Unprovisioned => Text::NetworkNotProvisioned,
            BlockingReason::InvalidNetworkConfiguration => Text::NetworkBuildInvalid,
            BlockingReason::TrustStateUnreadable => Text::TrustStateUnreadable,
            BlockingReason::TrustStateInvalid => Text::TrustStateInvalid,
            BlockingReason::TrustStateUnwritable => Text::TrustStateUnwritable,
            BlockingReason::NoRelayCandidates => Text::NoRelayCandidates,
            BlockingReason::DirectoryUnavailable | BlockingReason::AliasUnavailable => {
                Text::DirectoryUnavailable
            }
        };
        let retryable = matches!(
            blocking.recovery,
            RecoveryAction::Retry | RecoveryAction::RepairSettings
        );
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
        if retryable {
            actions = actions.child(
                Self::button(
                    "retry-share",
                    locale.text(if blocking.recovery == RecoveryAction::RepairSettings {
                        Text::UseSavedSettings
                    } else {
                        Text::RetryConnection
                    }),
                    Icon::Refresh,
                    ButtonStyle::Primary,
                    Action::ResumeShare(share.id),
                    cx,
                )
                .disabled(cooling_down || self.runtime.selection_disabled()),
            );
        }
        card = card.child(
            actions
                .child(Self::button(
                    "configure-relay",
                    locale.text(Text::SelfHostedRelay),
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
