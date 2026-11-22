use super::{live::RunError, should_show_failure_dialog};

#[test]
fn failure_dialog_is_restricted_to_non_diagnostic_launch_errors() {
    let launch = RunError::Launch("failed to initialize".into());
    let shutdown = RunError::Shutdown("engine cleanup failed".into());

    assert!(should_show_failure_dialog(&launch, false));
    assert!(!should_show_failure_dialog(&launch, true));
    assert!(!should_show_failure_dialog(&shutdown, false));
    assert!(!should_show_failure_dialog(&shutdown, true));
}
