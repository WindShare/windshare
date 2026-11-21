#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Route {
    Admin(AdminRoute),
    Internal(InternalRoute),
    Maintenance(MaintenanceRoute),
    Vantage(VantageRoute),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AdminRoute {
    Prepare,
    Stage,
    Promote,
    Manage,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum InternalRoute {
    Tick,
    Mirrors,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum MaintenanceRoute {
    Status,
    Drain,
    Release,
    Probe,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum VantageRoute {
    Targets,
    Cycle,
    Poll,
    Result,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PostCommitReconcile {
    None,
    /// Reconcile pending SQLite heads only after a directory-changing command commits.
    OnCommitPending,
    /// Audit KV even when the tick transaction fails so committed heads cannot strand.
    AlwaysAudit,
}

impl Route {
    pub const ALL: [Self; 14] = [
        Self::Admin(AdminRoute::Prepare),
        Self::Admin(AdminRoute::Stage),
        Self::Admin(AdminRoute::Promote),
        Self::Admin(AdminRoute::Manage),
        Self::Internal(InternalRoute::Tick),
        Self::Internal(InternalRoute::Mirrors),
        Self::Maintenance(MaintenanceRoute::Status),
        Self::Maintenance(MaintenanceRoute::Drain),
        Self::Maintenance(MaintenanceRoute::Release),
        Self::Maintenance(MaintenanceRoute::Probe),
        Self::Vantage(VantageRoute::Targets),
        Self::Vantage(VantageRoute::Cycle),
        Self::Vantage(VantageRoute::Poll),
        Self::Vantage(VantageRoute::Result),
    ];

    pub const fn path(self) -> &'static str {
        match self {
            Self::Admin(AdminRoute::Prepare) => "/internal/fleet/prepare",
            Self::Admin(AdminRoute::Stage) => "/internal/fleet/stage",
            Self::Admin(AdminRoute::Promote) => "/internal/fleet/promote",
            Self::Admin(AdminRoute::Manage) => "/v1/admin/nodes/manage",
            Self::Internal(InternalRoute::Tick) => "/internal/fleet/tick",
            Self::Internal(InternalRoute::Mirrors) => "/internal/fleet/mirrors",
            Self::Maintenance(MaintenanceRoute::Status) => "/v1/maintenance/status",
            Self::Maintenance(MaintenanceRoute::Drain) => "/v1/maintenance/drain",
            Self::Maintenance(MaintenanceRoute::Release) => "/v1/maintenance/release",
            Self::Maintenance(MaintenanceRoute::Probe) => "/v1/maintenance/probe",
            Self::Vantage(VantageRoute::Targets) => "/v1/vantage/targets",
            Self::Vantage(VantageRoute::Cycle) => "/v1/vantage/cycle",
            Self::Vantage(VantageRoute::Poll) => "/v1/vantage/poll",
            Self::Vantage(VantageRoute::Result) => "/v1/vantage/result",
        }
    }

    /// Resolve only explicitly supported fleet paths so unknown paths under fleet prefixes
    /// fall through to 404 instead of entering fleet transaction and directory import logic.
    pub fn parse(path: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|route| route.path() == path)
    }

    pub const fn reconcile_policy(self) -> PostCommitReconcile {
        match self {
            Self::Internal(InternalRoute::Tick) => PostCommitReconcile::AlwaysAudit,
            Self::Admin(AdminRoute::Promote | AdminRoute::Manage)
            | Self::Maintenance(MaintenanceRoute::Drain | MaintenanceRoute::Release) => {
                PostCommitReconcile::OnCommitPending
            }
            Self::Admin(AdminRoute::Prepare | AdminRoute::Stage)
            | Self::Internal(InternalRoute::Mirrors)
            | Self::Maintenance(MaintenanceRoute::Status | MaintenanceRoute::Probe)
            | Self::Vantage(
                VantageRoute::Targets
                | VantageRoute::Cycle
                | VantageRoute::Poll
                | VantageRoute::Result,
            ) => PostCommitReconcile::None,
        }
    }
}
