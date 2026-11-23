#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Route {
    Prepare,
    Enroll,
    Manage,
    Tick,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PostCommitReconcile {
    None,
    OnCommitPending,
    AlwaysAudit,
}

impl Route {
    pub const ALL: [Self; 4] = [Self::Prepare, Self::Enroll, Self::Manage, Self::Tick];

    pub const fn path(self) -> &'static str {
        match self {
            Self::Prepare => "/internal/fleet/prepare",
            Self::Enroll => "/internal/fleet/enroll",
            Self::Manage => "/v1/admin/nodes/manage",
            Self::Tick => "/internal/fleet/tick",
        }
    }

    pub fn parse(path: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|route| route.path() == path)
    }

    pub const fn reconcile_policy(self) -> PostCommitReconcile {
        match self {
            Self::Prepare => PostCommitReconcile::None,
            Self::Enroll | Self::Manage => PostCommitReconcile::OnCommitPending,
            Self::Tick => PostCommitReconcile::AlwaysAudit,
        }
    }
}
