use super::AccountPage;
use crate::{
    desktop::Desktop,
    flow_state::ShareFlow,
    live::byte_size,
    localization::{Language, Text, language},
    model::{Action, Page, SettingsSection, Share, ShareState},
};
use gpui::{Div, prelude::*, px};
use windshare_host::{AccountState, AccountStatus, AccountView, Allowance, ShareAuthorization};
use windshare_ui::{ButtonStyle, Icon, Theme, row};

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

pub fn account_status_summary(view: &AccountView, locale: Language) -> String {
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

impl Desktop {
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
                .child(theme.caption(account_status_summary(view, locale)))
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
}

#[cfg(test)]
#[path = "notice_tests.rs"]
mod tests;
