use crate::fleet_route::{
    AdminRoute, InternalRoute, MaintenanceRoute, PostCommitReconcile, Route, VantageRoute,
};
use std::collections::BTreeSet;

#[test]
fn all_fleet_routes_round_trip_with_unique_paths_and_expected_reconcile_policies() {
    let expected = [
        (
            "/internal/fleet/prepare",
            Route::Admin(AdminRoute::Prepare),
            PostCommitReconcile::None,
        ),
        (
            "/internal/fleet/stage",
            Route::Admin(AdminRoute::Stage),
            PostCommitReconcile::None,
        ),
        (
            "/internal/fleet/promote",
            Route::Admin(AdminRoute::Promote),
            PostCommitReconcile::OnCommitPending,
        ),
        (
            "/v1/admin/nodes/manage",
            Route::Admin(AdminRoute::Manage),
            PostCommitReconcile::OnCommitPending,
        ),
        (
            "/internal/fleet/tick",
            Route::Internal(InternalRoute::Tick),
            PostCommitReconcile::AlwaysAudit,
        ),
        (
            "/internal/fleet/mirrors",
            Route::Internal(InternalRoute::Mirrors),
            PostCommitReconcile::None,
        ),
        (
            "/v1/maintenance/status",
            Route::Maintenance(MaintenanceRoute::Status),
            PostCommitReconcile::None,
        ),
        (
            "/v1/maintenance/drain",
            Route::Maintenance(MaintenanceRoute::Drain),
            PostCommitReconcile::OnCommitPending,
        ),
        (
            "/v1/maintenance/release",
            Route::Maintenance(MaintenanceRoute::Release),
            PostCommitReconcile::OnCommitPending,
        ),
        (
            "/v1/maintenance/probe",
            Route::Maintenance(MaintenanceRoute::Probe),
            PostCommitReconcile::None,
        ),
        (
            "/v1/vantage/targets",
            Route::Vantage(VantageRoute::Targets),
            PostCommitReconcile::None,
        ),
        (
            "/v1/vantage/cycle",
            Route::Vantage(VantageRoute::Cycle),
            PostCommitReconcile::None,
        ),
        (
            "/v1/vantage/poll",
            Route::Vantage(VantageRoute::Poll),
            PostCommitReconcile::None,
        ),
        (
            "/v1/vantage/result",
            Route::Vantage(VantageRoute::Result),
            PostCommitReconcile::None,
        ),
    ];

    assert_eq!(Route::ALL.len(), expected.len());
    let mut paths = BTreeSet::new();
    for (path, route, policy) in expected {
        assert!(paths.insert(path), "duplicate fleet route path {path}");
        assert_eq!(route.path(), path);
        assert_eq!(Route::parse(path), Some(route));
        assert_eq!(route.reconcile_policy(), policy);
    }
    for route in Route::ALL {
        assert_eq!(Route::parse(route.path()), Some(route));
    }
}

#[test]
fn unknown_paths_under_fleet_prefixes_do_not_match() {
    for path in [
        "",
        "/v1/vantage",
        "/v1/vantage/",
        "/v1/vantage/unknown",
        "/v1/vantage/targets/",
        "/v1/vantage/targets/extra",
        "/v1/maintenance",
        "/v1/maintenance/",
        "/v1/maintenance/unknown",
        "/v1/maintenance/status/",
        "/internal/fleet",
        "/internal/fleet/",
        "/internal/fleet/unknown",
        "/internal/fleet/tick/",
        "/v1/admin/nodes/manage/",
        "/v1/admin/nodes/other",
        "/v1/accounts",
    ] {
        assert_eq!(Route::parse(path), None, "unexpected match for {path}");
    }
}
