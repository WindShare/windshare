//! The durability barrier shared by fresh publication and ownership replacement.

use crate::clock::UnixMillis;

use super::{
    Action, ConnectionRef, Observer, OperationId, OwnedRoute, PublishedRegistration, Record,
    Refusal, ShareLifecycle, State,
};

/// An authenticated ownership change whose exact record must be durable before
/// JOIN, forwarding or REGISTERED can use it. Retries retain this identity.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct OwnershipCommit {
    pub(super) id: OperationId,
    pub(super) route: OwnedRoute,
    pub(super) deadline: UnixMillis,
    pub(super) replaced: Option<ConnectionRef>,
}

impl OwnershipCommit {
    #[must_use]
    pub const fn id(self) -> OperationId {
        self.id
    }

    #[must_use]
    pub const fn record(self) -> Record {
        Record::Published(self.route)
    }

    /// The complete owner stored atomically with a fresh descriptor.
    #[must_use]
    pub const fn ownership(self) -> OwnedRoute {
        self.route
    }

    /// The original handshake budget also bounds the final durable confirmation.
    #[must_use]
    pub const fn deadline(self) -> UnixMillis {
        self.deadline
    }

    /// Close this exact former owner after the write, even if STOP or a
    /// disconnect has since superseded the pending activation.
    #[must_use]
    pub const fn replaced(self) -> Option<ConnectionRef> {
        self.replaced
    }

    pub(super) const fn registration(self) -> PublishedRegistration {
        PublishedRegistration {
            owner: self.route.ownership(),
            route: self.route.route,
        }
    }
}

impl<O: Observer> ShareLifecycle<O> {
    pub(super) const fn activate(
        &mut self,
        id: OperationId,
        registration: PublishedRegistration,
        deadline: UnixMillis,
        replaced: Option<ConnectionRef>,
    ) -> OwnershipCommit {
        let commit = OwnershipCommit {
            id,
            route: OwnedRoute {
                route: registration.route,
                epoch: registration.owner.epoch,
                owner: registration.owner.connection,
            },
            deadline,
            replaced,
        };
        self.state = State::Activating(commit);
        commit
    }

    pub(super) fn ownership_committed(&mut self, id: OperationId) -> Action<'static> {
        let State::Activating(commit) = self.state else {
            return Action::Refused(Refusal::StaleOperation);
        };
        if commit.id != id {
            return Action::Refused(Refusal::StaleOperation);
        }
        let registration = commit.registration();
        self.state = State::Published(registration);
        Action::Registered(registration)
    }
}
