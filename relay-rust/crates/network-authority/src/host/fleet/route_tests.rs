use crate::fleet_route::{PostCommitReconcile, Route};

#[test]
fn only_directory_mutations_and_ticks_reconcile_kv() {
    for (route, policy) in [
        (Route::Prepare, PostCommitReconcile::None),
        (Route::Manage, PostCommitReconcile::OnCommitPending),
        (Route::Enroll, PostCommitReconcile::OnCommitPending),
        (Route::Tick, PostCommitReconcile::AlwaysAudit),
    ] {
        assert_eq!(Route::parse(route.path()), Some(route));
        assert_eq!(route.reconcile_policy(), policy);
    }
    assert_eq!(Route::parse("/internal/fleet/unknown"), None);
}
