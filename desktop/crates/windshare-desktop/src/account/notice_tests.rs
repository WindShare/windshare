use super::*;
use crate::model::{ShareAge, ShareContents, ShareSource};
use windshare_host::AccountProfile;
use windshare_ui::{Icon, Tone};

fn account_status(active: bool, state: AccountState, allowance: Allowance) -> AccountStatus {
    AccountStatus {
        active,
        state,
        email_code_pending: false,
        account: String::new(),
        credential: String::new(),
        username: String::new(),
        profile: AccountProfile::default(),
        pending_email: None,
        allowance,
        remaining_bytes: 0,
        reset_at: String::new(),
        reason: String::new(),
    }
}

fn share(state: ShareState, authorization: Option<ShareAuthorization>) -> Share {
    Share {
        id: 1,
        source: ShareSource::Native {
            operation_id: "op-1".into(),
        },
        name: "archive.tar".into(),
        link: None,
        contents: ShareContents::Selected(1),
        size: None,
        age: ShareAge::JustNow,
        tone: Tone::Mint,
        symbol: Icon::File,
        state,
        error: None,
        authorization,
        relay_status: None,
    }
}

#[test]
fn transient_background_states_do_not_flash_ambient_notice() {
    let preparing = Some(ActiveShareDemand {
        preparing: true,
        official_auth_wait: false,
    });
    for (state, allowance) in [
        (AccountState::PendingSetup, Allowance::Stale),
        (AccountState::RefreshRequired, Allowance::Stale),
        (AccountState::Refreshing, Allowance::Stale),
        (AccountState::Ready, Allowance::Stale),
        (AccountState::Ready, Allowance::Available),
    ] {
        let status = account_status(true, state, allowance);
        assert!(
            !should_show_account_notice(&status, AccountNoticeSurface::Ambient, preparing),
            "expected transient or healthy state {state:?}/{allowance:?} to stay silent"
        );
    }
}

#[test]
fn authorization_prompts_require_blocked_share_and_yield_to_owning_surfaces() {
    let blocked = Some(ActiveShareDemand {
        preparing: false,
        official_auth_wait: true,
    });
    let live_only = Some(ActiveShareDemand::default());
    let preparing_only = Some(ActiveShareDemand {
        preparing: true,
        official_auth_wait: false,
    });

    for state in [
        AccountState::EmailRequired,
        AccountState::RegistrationRequired,
    ] {
        let status = account_status(true, state, Allowance::Stale);
        assert!(should_show_account_notice(
            &status,
            AccountNoticeSurface::Ambient,
            blocked
        ));
        assert!(!should_show_account_notice(
            &status,
            AccountNoticeSurface::OfficialAuthorizationCard,
            blocked
        ));
        assert!(!should_show_account_notice(
            &status,
            AccountNoticeSurface::AccountSettings,
            blocked
        ));
        assert!(!should_show_account_notice(
            &status,
            AccountNoticeSurface::Ambient,
            live_only
        ));
        assert!(!should_show_account_notice(
            &status,
            AccountNoticeSurface::Ambient,
            preparing_only
        ));
    }
}

#[test]
fn registering_progress_banner_only_tracks_preparing_shares() {
    let status = account_status(
        true,
        AccountState::Registering {
            registration_permille: 420,
        },
        Allowance::Stale,
    );
    let preparing = Some(ActiveShareDemand {
        preparing: true,
        official_auth_wait: false,
    });
    assert!(should_show_account_notice(
        &status,
        AccountNoticeSurface::Ambient,
        preparing
    ));
    assert!(!should_show_account_notice(
        &status,
        AccountNoticeSurface::Ambient,
        Some(ActiveShareDemand::default())
    ));
    assert!(!should_show_account_notice(
        &status,
        AccountNoticeSurface::AccountSettings,
        preparing
    ));
}

#[test]
fn degraded_and_denied_states_surface_on_ambient_pages_only() {
    let active_live = Some(ActiveShareDemand::default());
    let blocked = Some(ActiveShareDemand {
        preparing: false,
        official_auth_wait: true,
    });
    for (state, allowance) in [
        (AccountState::Ready, Allowance::Spent),
        (AccountState::TemporarilyUnavailable, Allowance::Stale),
        (AccountState::StorageError, Allowance::Stale),
        (AccountState::Denied, Allowance::Stale),
    ] {
        let status = account_status(true, state, allowance);
        assert!(should_show_account_notice(
            &status,
            AccountNoticeSurface::Ambient,
            active_live
        ));
        assert!(!should_show_account_notice(
            &status,
            AccountNoticeSurface::OfficialAuthorizationCard,
            blocked
        ));
        assert!(!should_show_account_notice(
            &status,
            AccountNoticeSurface::AccountSettings,
            active_live
        ));
        assert!(!should_show_account_notice(
            &status,
            AccountNoticeSurface::Ambient,
            None
        ));
        let inactive = account_status(false, state, allowance);
        assert!(!should_show_account_notice(
            &inactive,
            AccountNoticeSurface::Ambient,
            active_live
        ));
    }
}

#[test]
fn official_authorization_wait_excludes_self_hosted_and_non_waiting_shares() {
    assert!(is_official_authorization_wait(&share(
        ShareState::AwaitingAuthorization,
        Some(ShareAuthorization::Official),
    )));
    assert!(!is_official_authorization_wait(&share(
        ShareState::AwaitingAuthorization,
        Some(ShareAuthorization::SelfHosted {
            relay_url: "https://relay.example".into(),
        }),
    )));
    assert!(!is_official_authorization_wait(&share(
        ShareState::AwaitingAuthorization,
        None,
    )));
    assert!(!is_official_authorization_wait(&share(
        ShareState::Preparing,
        Some(ShareAuthorization::Official),
    )));
}
