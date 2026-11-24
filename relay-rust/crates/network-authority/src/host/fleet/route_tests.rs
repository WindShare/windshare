use crate::fleet_route::{PostCommitReconcile, Route};

#[test]
fn only_directory_mutations_and_ticks_reconcile_kv() {
    for route in Route::ALL {
        assert_eq!(Route::parse(route.path()), Some(route));
        let expected = match route {
            Route::Prepare => PostCommitReconcile::None,
            Route::Enroll | Route::Manage => PostCommitReconcile::OnCommitPending,
            Route::Tick => PostCommitReconcile::AlwaysAudit,
        };
        assert_eq!(route.reconcile_policy(), expected);
    }
    assert_eq!(Route::parse("/internal/fleet/unknown"), None);
}
