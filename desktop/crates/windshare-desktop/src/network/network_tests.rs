use super::{NetworkSettingsState, SetupDialogHandoff, settings_visible_setup};
use crate::localization::Text;
use windshare_host::{
    Failure, Model, RelayOperation, SavedRelay, Setup, SetupContext, SetupRecovery, SetupRequest,
};

fn global_context(id: &str) -> SetupContext {
    SetupContext {
        id: id.into(),
        operation_id: String::new(),
        frontend_saved: false,
        relay_url: "https://dead.invalid".into(),
        relay_operation: Some(RelayOperation::AddProfile),
        relay_saved: false,
    }
}

fn failure(message: &str) -> Failure {
    Failure::new("setup_failed", message)
}

#[test]
fn handoff_surfaces_terminal_failure_after_dialog_closes() {
    let mut state = NetworkSettingsState::default();
    state.adopt_dialog_handoff(
        SetupDialogHandoff {
            in_flight_id: Some("setup:1".into()),
            remove_toast_id: None,
        },
        true,
    );

    let failed = Model {
        setup: Setup::Failed {
            context: global_context("setup:1"),
            recovery: Some(SetupRecovery::Restart),
            error: failure("relay unreachable"),
        },
        setup_request: SetupRequest::Failed {
            id: "setup:1".into(),
            error: failure("relay unreachable"),
        },
        ..Model::default()
    };

    assert!(settings_visible_setup(&failed.setup).is_none());
    assert!(matches!(
        state.inline_feedback(&failed),
        Some(Err(ref message)) if message == "relay unreachable"
    ));
}

#[test]
fn handoff_surfaces_success_and_remove_toast_when_dismissed_in_flight() {
    let mut state = NetworkSettingsState::default();
    state.adopt_dialog_handoff(
        SetupDialogHandoff {
            in_flight_id: Some("setup:2".into()),
            remove_toast_id: Some("setup:2".into()),
        },
        true,
    );

    let mut context = global_context("setup:2");
    context.relay_saved = true;
    let mut finished = Model {
        setup: Setup::Finished { context },
        setup_request: SetupRequest::Finished {
            id: "setup:2".into(),
        },
        ..Model::default()
    };
    finished.preferences.relays.push(SavedRelay {
        url: "https://dead.invalid".into(),
        enabled: true,
        console_url: None,
        offered_frontend: None,
    });

    assert!(matches!(
        state.inline_feedback(&finished),
        Some(Ok(Text::RelayConnected))
    ));
    assert!(matches!(
        state.take_remove_toast(&finished),
        Some(Text::RelayRemoved)
    ));
    assert!(state.take_remove_toast(&finished).is_none());
}

#[test]
fn empty_handoff_or_off_page_dismissal_does_not_leak_inline_feedback() {
    let mut state = NetworkSettingsState::default();
    state.adopt_dialog_handoff(SetupDialogHandoff::default(), true);
    state.adopt_dialog_handoff(
        SetupDialogHandoff {
            in_flight_id: Some("setup:3".into()),
            remove_toast_id: Some("setup:3".into()),
        },
        false,
    );

    let failed = Model {
        setup: Setup::Failed {
            context: global_context("setup:3"),
            recovery: Some(SetupRecovery::Restart),
            error: failure("dial tcp failed"),
        },
        setup_request: SetupRequest::Failed {
            id: "setup:3".into(),
            error: failure("dial tcp failed"),
        },
        ..Model::default()
    };

    assert!(state.inline_feedback(&failed).is_none());
    assert_eq!(state.remove_toast_id.as_deref(), Some("setup:3"));
}

#[test]
fn observe_active_setup_tracks_only_settings_visible_unfinished_setups() {
    let running_global = Model {
        setup: Setup::Running {
            context: global_context("setup:4"),
            stage: "discover".into(),
            verification_url: String::new(),
            user_code: String::new(),
            service_login: None,
        },
        ..Model::default()
    };
    let mut state = NetworkSettingsState::default();
    state.observe_active_setup(&running_global);
    assert_eq!(state.inline_request_id.as_deref(), Some("setup:4"));

    // Navigating back after a share-scoped setup is running must not adopt it.
    let mut share_context = global_context("setup:5");
    share_context.operation_id = "share:1".into();
    let running_share = Model {
        setup: Setup::Running {
            context: share_context.clone(),
            stage: "discover".into(),
            verification_url: String::new(),
            user_code: String::new(),
            service_login: None,
        },
        ..Model::default()
    };
    state.clear_transient();
    state.observe_active_setup(&running_share);
    assert_eq!(state.inline_request_id, None);

    // A cancellation-blocking setup is shown inline on settings and stays tracked,
    // without duplicating the error below the banner while still unfinished.
    let cancel_required = Model {
        setup: Setup::Failed {
            context: share_context,
            recovery: Some(SetupRecovery::Cancel),
            error: failure("settings changed"),
        },
        setup_request: SetupRequest::Failed {
            id: "setup:5".into(),
            error: failure("settings changed"),
        },
        ..Model::default()
    };
    state.observe_active_setup(&cancel_required);
    assert_eq!(state.inline_request_id.as_deref(), Some("setup:5"));
    assert!(state.inline_feedback(&cancel_required).is_none());

    // Already-settled setups must not resurface stale errors on navigation.
    let settled_failure = Model {
        setup: Setup::Failed {
            context: global_context("setup:6"),
            recovery: Some(SetupRecovery::Restart),
            error: failure("stale error"),
        },
        setup_request: SetupRequest::Failed {
            id: "setup:6".into(),
            error: failure("stale error"),
        },
        ..Model::default()
    };
    state.clear_transient();
    state.observe_active_setup(&settled_failure);
    assert_eq!(state.inline_request_id, None);
    assert!(state.inline_feedback(&settled_failure).is_none());
}
