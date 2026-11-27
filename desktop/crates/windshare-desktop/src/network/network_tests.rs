use super::{
    CommunityNoticeState, NetworkSettingsState, SettingsSetupBanner,
    dialog_host::{
        DialogCloseAction, dialog_close_action, operation_finished, other_running_for_model,
    },
    relay_removed_toast, settings_setup_banner, settings_visible_setup,
};
use crate::{localization::Text, model::SharingSetupTarget};
use windshare_host::{
    BlockingReason, Completion, CreationBlock, Failure, Model, Operation, OperationState, Outcome,
    RecoveryAction, RelayOperation, SavedRelay, Setup, SetupContext, SetupRecovery, SetupRequest,
    SetupRequestKind, Share, ShareAuthorization, ShareState, StopReason,
};

fn global_context(id: &str) -> SetupContext {
    SetupContext {
        id: id.into(),
        operation_id: String::new(),
        frontend_saved: false,
        relay_url: "https://dead.invalid".into(),
        relay_operation: Some(RelayOperation::AddProfile),
        relay_saved: false,
        relay_removed: false,
    }
}

fn failure(message: &str) -> Failure {
    Failure::new("setup_failed", message)
}

#[test]
fn observe_active_setup_adopts_pending_global_setup_before_running_arrives() {
    let mut state = NetworkSettingsState::default();
    let pending_global = Model {
        setup_request: SetupRequest::Pending {
            id: "setup:1".into(),
            operation_id: String::new(),
            kind: SetupRequestKind::Configure,
        },
        ..Model::default()
    };
    assert_eq!(state.observe_active_setup(&pending_global), Some("setup:1"));
    assert_eq!(state.observe_active_setup(&pending_global), None);

    let failed = Model {
        setup: Setup::Failed {
            context: global_context("setup:1"),
            recovery: Some(SetupRecovery::Restart),
            error: failure("relay unreachable"),
        },
        ..Model::default()
    };
    assert!(settings_visible_setup(&failed.setup).is_none());
    assert!(matches!(
        state.inline_feedback(&failed),
        Some(Err(ref message)) if message == "relay unreachable"
    ));

    // A pending global setup that succeeds also surfaces inline completion feedback.
    state.clear_transient();
    let pending_second = Model {
        setup_request: SetupRequest::Pending {
            id: "setup:2".into(),
            operation_id: String::new(),
            kind: SetupRequestKind::Configure,
        },
        ..Model::default()
    };
    assert_eq!(state.observe_active_setup(&pending_second), Some("setup:2"));
    let mut context = global_context("setup:2");
    context.relay_saved = true;
    let mut finished = Model {
        setup: Setup::Finished { context },
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
}

#[test]
fn observe_active_setup_tracks_only_settings_visible_unfinished_setups() {
    let running_global = Model {
        setup: Setup::Running {
            context: global_context("setup:4"),
            stage: "discover".into(),
            verification_url: String::new(),
            user_code: String::new(),
            guest_available: false,
            service_login: None,
        },
        ..Model::default()
    };
    let mut state = NetworkSettingsState::default();
    assert_eq!(state.observe_active_setup(&running_global), Some("setup:4"));
    assert_eq!(state.inline_request_id.as_deref(), Some("setup:4"));

    // Settling an adopted running setup without a local SetupRequest still surfaces feedback.
    let mut finished_ctx = global_context("setup:4");
    finished_ctx.relay_saved = true;
    let mut finished_global = Model {
        setup: Setup::Finished {
            context: finished_ctx,
        },
        ..Model::default()
    };
    finished_global.preferences.relays.push(SavedRelay {
        url: "https://dead.invalid".into(),
        enabled: true,
        console_url: None,
        offered_frontend: None,
    });
    assert!(matches!(
        state.inline_feedback(&finished_global),
        Some(Ok(Text::RelayConnected))
    ));

    // Navigating back while a share-scoped setup is pending or running must not adopt it.
    let pending_share = Model {
        setup_request: SetupRequest::Pending {
            id: "setup:5".into(),
            operation_id: "share:1".into(),
            kind: SetupRequestKind::Configure,
        },
        ..Model::default()
    };
    state.clear_transient();
    assert_eq!(state.observe_active_setup(&pending_share), None);
    assert_eq!(state.inline_request_id, None);

    let mut share_context = global_context("setup:5");
    share_context.operation_id = "share:1".into();
    let running_share = Model {
        setup: Setup::Running {
            context: share_context.clone(),
            stage: "discover".into(),
            verification_url: String::new(),
            user_code: String::new(),
            guest_available: false,
            service_login: None,
        },
        ..Model::default()
    };
    assert_eq!(state.observe_active_setup(&running_share), None);
    assert_eq!(state.inline_request_id, None);

    // A cancellation-blocking setup is shown inline on settings and stays tracked,
    // without duplicating the error below the banner while still unfinished.
    let cancel_required = Model {
        setup: Setup::Failed {
            context: share_context.clone(),
            recovery: Some(SetupRecovery::Cancel),
            error: failure("settings changed"),
        },
        ..Model::default()
    };
    assert_eq!(
        state.observe_active_setup(&cancel_required),
        Some("setup:5")
    );
    assert_eq!(state.inline_request_id.as_deref(), Some("setup:5"));
    assert!(state.inline_feedback(&cancel_required).is_none());

    // Cancelling the owning share settles the host setup directly to Cancelled;
    // settings must report cancellation instead of a stale failure.
    let cancelled_share = Model {
        setup: Setup::Cancelled {
            context: share_context,
        },
        ..Model::default()
    };
    assert!(matches!(
        state.inline_feedback(&cancelled_share),
        Some(Ok(Text::SetupCancelled))
    ));

    // Already-settled setups must not resurface stale errors on navigation.
    let settled_failure = Model {
        setup: Setup::Failed {
            context: global_context("setup:6"),
            recovery: Some(SetupRecovery::Restart),
            error: failure("stale error"),
        },
        ..Model::default()
    };
    state.clear_transient();
    assert_eq!(state.observe_active_setup(&settled_failure), None);
    assert_eq!(state.inline_request_id, None);
    assert!(state.inline_feedback(&settled_failure).is_none());

    // Local command rejection that never reached Model::setup still renders inline.
    state.inline_request_id = Some("setup:7".into());
    let local_reject = Model {
        setup_request: SetupRequest::Rejected {
            id: "setup:7".into(),
            error: failure("WindShare is closing"),
        },
        ..Model::default()
    };
    assert!(matches!(
        state.inline_feedback(&local_reject),
        Some(Err(ref message)) if message == "WindShare is closing"
    ));
}

#[test]
fn relay_removed_toast_fires_once_per_finished_removal_regardless_of_scope() {
    let idle = Model::default();

    let mut global_removed_ctx = global_context("setup:10");
    global_removed_ctx.relay_url.clear();
    global_removed_ctx.relay_operation = None;
    global_removed_ctx.relay_removed = true;
    let global_removed = Model {
        setup: Setup::Finished {
            context: global_removed_ctx,
        },
        ..Model::default()
    };
    assert!(matches!(
        relay_removed_toast(&idle, &global_removed),
        Some(Text::RelayRemoved)
    ));
    // Later unrelated snapshots carrying the same finished setup must not re-fire the toast.
    assert!(relay_removed_toast(&global_removed, &global_removed).is_none());

    // Removals triggered from RecoverShare carry a non-empty operation_id and still notify
    // even when consecutive removals coalesce without an intermediate Running snapshot.
    let mut share_removed_ctx = global_context("setup:11");
    share_removed_ctx.operation_id = "share:1".into();
    share_removed_ctx.relay_url.clear();
    share_removed_ctx.relay_operation = None;
    share_removed_ctx.relay_removed = true;
    let share_removed = Model {
        setup: Setup::Finished {
            context: share_removed_ctx,
        },
        ..Model::default()
    };
    assert!(matches!(
        relay_removed_toast(&global_removed, &share_removed),
        Some(Text::RelayRemoved)
    ));
    assert!(relay_removed_toast(&share_removed, &share_removed).is_none());

    // Non-removal completions and failed removals must not trigger the removed toast.
    let mut saved_ctx = global_context("setup:12");
    saved_ctx.relay_saved = true;
    let saved = Model {
        setup: Setup::Finished { context: saved_ctx },
        ..Model::default()
    };
    assert!(relay_removed_toast(&share_removed, &saved).is_none());

    let mut failed_ctx = global_context("setup:13");
    failed_ctx.relay_removed = false;
    let failed = Model {
        setup: Setup::Failed {
            context: failed_ctx,
            recovery: Some(SetupRecovery::Restart),
            error: failure("relay is not saved"),
        },
        ..Model::default()
    };
    assert!(relay_removed_toast(&saved, &failed).is_none());
}

#[test]
fn blocked_share_dialog_does_not_adopt_another_shares_running_setup() {
    let mut context = global_context("setup:7");
    context.operation_id = "share:a".into();
    let model = Model {
        setup: Setup::Running {
            context,
            stage: "discover".into(),
            verification_url: String::new(),
            user_code: String::new(),
            guest_available: false,
            service_login: None,
        },
        setup_request: SetupRequest::Pending {
            id: "setup:7".into(),
            operation_id: "share:a".into(),
            kind: SetupRequestKind::Configure,
        },
        ..Model::default()
    };

    assert!(other_running_for_model(&model, "share:b").is_some());
    assert_eq!(model.in_flight_setup_id("share:b"), None);

    assert!(other_running_for_model(&model, "share:a").is_none());
    assert_eq!(model.in_flight_setup_id("share:a"), Some("setup:7"));

    assert!(other_running_for_model(&model, "").is_none());
    assert_eq!(model.in_flight_setup_id(""), Some("setup:7"));
}

fn operation_with_state(id: &str, state: ShareState) -> Operation {
    Operation {
        id: id.into(),
        paths: Vec::new(),
        state: OperationState::Accepted(Box::new(Share {
            operation_id: id.into(),
            created_at_ms: 0,
            selection: None,
            omissions: None,
            state,
        })),
    }
}

fn blocked_state() -> ShareState {
    ShareState::AwaitingConfiguration {
        blocking: CreationBlock {
            reason: BlockingReason::ConfigurationRequired,
            recovery: RecoveryAction::Configure,
            retry_at_ms: None,
        },
        error: None,
    }
}

fn finished_state(outcome: Outcome) -> ShareState {
    ShareState::Finished {
        result: Completion {
            outcome,
            stop_reason: if outcome == Outcome::Cancelled {
                StopReason::Cancelled
            } else {
                StopReason::None
            },
            error: String::new(),
            cleanup_error: String::new(),
        },
        error: None,
        authorization: None,
    }
}

#[test]
fn operation_finished_distinguishes_active_and_terminal_operations() {
    let model = Model {
        operations: vec![
            Operation {
                id: "op:submitting".into(),
                paths: Vec::new(),
                state: OperationState::Submitting,
            },
            Operation {
                id: "op:rejected".into(),
                paths: Vec::new(),
                state: OperationState::Rejected(failure("bad selection")),
            },
            operation_with_state("op:blocked", blocked_state()),
            operation_with_state("op:finished", finished_state(Outcome::Cancelled)),
        ],
        ..Model::default()
    };

    assert!(!operation_finished(&model, ""));
    assert!(!operation_finished(&model, "op:missing"));
    assert!(!operation_finished(&model, "op:submitting"));
    assert!(!operation_finished(&model, "op:blocked"));
    assert!(operation_finished(&model, "op:rejected"));
    assert!(operation_finished(&model, "op:finished"));
}

#[test]
fn share_scoped_dialog_stays_open_on_setup_cancel_and_closes_when_share_finishes() {
    let blocked = Model {
        operations: vec![operation_with_state("op:1", blocked_state())],
        ..Model::default()
    };
    let mut cancelled_ctx = global_context("setup:1");
    cancelled_ctx.operation_id = "op:1".into();
    let setup_cancelled_share_blocked = Model {
        operations: vec![operation_with_state("op:1", blocked_state())],
        setup: Setup::Cancelled {
            context: cancelled_ctx,
        },
        ..Model::default()
    };
    assert_eq!(
        dialog_close_action(
            SharingSetupTarget::RecoverShare(1),
            &blocked,
            &setup_cancelled_share_blocked,
            "op:1",
            false,
        ),
        DialogCloseAction::StayOpen
    );

    let preparing = Model {
        operations: vec![operation_with_state(
            "op:1",
            ShareState::Preparing {
                task_id: "task:1".into(),
                error: None,
            },
        )],
        ..Model::default()
    };
    assert_eq!(
        dialog_close_action(
            SharingSetupTarget::RecoverShare(1),
            &blocked,
            &preparing,
            "op:1",
            true,
        ),
        DialogCloseAction::CompleteWithToast
    );

    let share_cancelled = Model {
        operations: vec![operation_with_state(
            "op:1",
            finished_state(Outcome::Cancelled),
        )],
        ..Model::default()
    };
    assert_eq!(
        dialog_close_action(
            SharingSetupTarget::RecoverShare(1),
            &blocked,
            &share_cancelled,
            "op:1",
            false,
        ),
        DialogCloseAction::CloseWithoutToast
    );
    assert_eq!(
        dialog_close_action(
            SharingSetupTarget::SwitchRelay(1),
            &blocked,
            &share_cancelled,
            "op:1",
            false,
        ),
        DialogCloseAction::CloseWithoutToast
    );

    // Even if an authorization setup finishes in the same snapshot as a terminal share
    // failure, closing without a green setup toast exposes the share failure cleanly.
    let awaiting_auth = Model {
        operations: vec![operation_with_state(
            "op:1",
            ShareState::AwaitingAuthorization {
                authorization: ShareAuthorization::SelfHosted {
                    relay_url: "https://relay.example".into(),
                },
            },
        )],
        ..Model::default()
    };
    let mut auth_ctx = global_context("setup:2");
    auth_ctx.operation_id = "op:1".into();
    auth_ctx.relay_operation = Some(RelayOperation::AuthorizeDevice);
    auth_ctx.relay_saved = true;
    let auth_done_share_failed = Model {
        operations: vec![operation_with_state(
            "op:1",
            finished_state(Outcome::Failed),
        )],
        setup: Setup::Finished { context: auth_ctx },
        ..Model::default()
    };
    assert_eq!(
        dialog_close_action(
            SharingSetupTarget::RecoverShare(1),
            &awaiting_auth,
            &auth_done_share_failed,
            "op:1",
            true,
        ),
        DialogCloseAction::CloseWithoutToast
    );

    // Global Defaults target closes on setup finish and ignores share state.
    assert_eq!(
        dialog_close_action(
            SharingSetupTarget::Defaults,
            &blocked,
            &share_cancelled,
            "",
            false,
        ),
        DialogCloseAction::StayOpen
    );
    assert_eq!(
        dialog_close_action(
            SharingSetupTarget::Defaults,
            &blocked,
            &share_cancelled,
            "",
            true,
        ),
        DialogCloseAction::CompleteWithToast
    );
}

#[test]
fn settings_setup_banner_routes_share_scoped_unfinished_setups_to_return_card() {
    let mut share_ctx = global_context("setup:20");
    share_ctx.operation_id = "share:1".into();

    // Settled or idle states render no banner.
    assert_eq!(settings_setup_banner(&Setup::Idle), None);
    assert_eq!(
        settings_setup_banner(&Setup::Finished {
            context: share_ctx.clone(),
        }),
        None
    );
    assert_eq!(
        settings_setup_banner(&Setup::Cancelled {
            context: share_ctx.clone(),
        }),
        None
    );
    for settled in [SetupRecovery::Restart, SetupRecovery::Blocked] {
        let failed = Setup::Failed {
            context: share_ctx.clone(),
            recovery: Some(settled),
            error: failure("settled"),
        };
        assert_eq!(settings_setup_banner(&failed), None);
    }

    // Global setups and cancellation-blocking share setups stay inline.
    let global_running = Setup::Running {
        context: global_context("setup:21"),
        stage: "discover".into(),
        verification_url: String::new(),
        user_code: String::new(),
        guest_available: false,
        service_login: None,
    };
    assert_eq!(
        settings_setup_banner(&global_running),
        Some(SettingsSetupBanner::InlineProgress)
    );
    for inline_share in [
        Setup::Cancelling {
            context: share_ctx.clone(),
        },
        Setup::CancelFailed {
            context: share_ctx.clone(),
            error: failure("cancel failed"),
        },
        Setup::Failed {
            context: share_ctx.clone(),
            recovery: Some(SetupRecovery::Cancel),
            error: failure("settings changed"),
        },
        Setup::Failed {
            context: share_ctx.clone(),
            recovery: Some(SetupRecovery::RetryCancel),
            error: failure("retry cancel"),
        },
    ] {
        assert_eq!(
            settings_setup_banner(&inline_share),
            Some(SettingsSetupBanner::InlineProgress)
        );
    }

    // Share-scoped unfinished setups that do not require cancellation surface a
    // return card without adopting the setup into Settings' inline feedback.
    let share_running = Setup::Running {
        context: share_ctx.clone(),
        stage: "approval".into(),
        verification_url: "https://relay.example/approve".into(),
        user_code: "ABCD-1234".into(),
        guest_available: false,
        service_login: None,
    };
    assert!(settings_visible_setup(&share_running).is_none());
    assert_eq!(
        settings_setup_banner(&share_running),
        Some(SettingsSetupBanner::ReturnToShare(&share_ctx))
    );

    for recovery in [
        Some(SetupRecovery::Continue),
        Some(SetupRecovery::Unknown),
        None,
    ] {
        let share_failed = Setup::Failed {
            context: share_ctx.clone(),
            recovery,
            error: failure("interrupted"),
        };
        assert!(settings_visible_setup(&share_failed).is_none());
        assert_eq!(
            settings_setup_banner(&share_failed),
            Some(SettingsSetupBanner::ReturnToShare(&share_ctx))
        );
    }
}

#[test]
fn community_notice_state_surfaces_initial_latch_and_tracks_dismissal() {
    let mut state = CommunityNoticeState::default();
    assert!(!state.visible());
    assert!(!state.observe(0));
    assert!(!state.visible());

    // A notice latched in the initial host model before ModelCursor attaches
    // still transitions from seen=0 -> 1 and becomes visible.
    assert!(state.observe(1));
    assert!(state.visible());
    assert!(!state.observe(1));

    // Dismissing clears visibility without forgetting the seen counter.
    assert!(state.dismiss());
    assert!(!state.visible());
    assert!(!state.dismiss());
    assert!(!state.observe(1));
    assert!(!state.visible());

    // A subsequent pause cycle (re-enabled then disabled again) increments the
    // host counter and surfaces the notice once more.
    assert!(state.observe(2));
    assert!(state.visible());
}
