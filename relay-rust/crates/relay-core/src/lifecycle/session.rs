//! Binds receiver sessions to the same authority that commits STOP and RESUME.

use crate::{
    clock::UnixMillis,
    identifiers::RelaySessionId,
    join::JoinRequest,
    sessions::{ForwardRefusal, RetirementReason, Revocation, Session},
};

use super::{Action, Observer, OwnershipId, PublishedRegistration, Refusal, ShareLifecycle, State};

/// One admitted receiver and the exact descriptor source it joined.
#[derive(Debug, Eq, PartialEq)]
pub struct Joined {
    /// The host delivers this published route's opaque descriptor.
    pub registration: PublishedRegistration,
    /// Move this authority into the host's bounded session storage.
    pub session: Session,
}

impl<O: Observer> ShareLifecycle<O> {
    pub(crate) const fn ownership_record(&self) -> Option<super::Record> {
        match self.state {
            State::Published(registration) => Some(registration.record()),
            State::Grace(route) => Some(route.record()),
            _ => None,
        }
    }

    #[expect(
        clippy::needless_pass_by_value,
        reason = "JOIN consumes the connection-bound handshake outcome"
    )]
    pub(super) fn join_session(
        &mut self,
        request: JoinRequest,
        wire_id: RelaySessionId,
        now: UnixMillis,
    ) -> Action<'static> {
        if request.relay() != self.relay {
            return Action::Refused(Refusal::WrongRelay);
        }
        if now >= request.deadline() {
            return Action::Refused(Refusal::JoinExpired);
        }
        let registration = match self.join(request.share()) {
            Action::JoinReady(registration) => registration,
            refused => return refused,
        };
        if request.connection() == registration.owner.connection {
            return Action::Refused(Refusal::InvalidReceiver);
        }
        let Some(sequence) = self.next_sequence() else {
            return Action::Refused(Refusal::SequenceExhausted);
        };
        let admission = self.operation(request.connection(), sequence);
        Action::Joined(Joined {
            registration,
            session: Session::new(admission, registration, wire_id, request.deadline()),
        })
    }

    pub(super) const fn session_owner(&self) -> Option<OwnershipId> {
        match self.state {
            State::Published(published) => Some(published.owner),
            _ => None,
        }
    }

    pub(crate) fn check_session_owner(&self, owner: OwnershipId) -> Result<(), ForwardRefusal> {
        if owner.relay != self.relay || owner.share != self.share {
            return Err(ForwardRefusal::ForeignLifecycle);
        }
        if self.session_owner() == Some(owner) {
            return Ok(());
        }
        Err(ForwardRefusal::Retired(self.session_revocation_reason()))
    }

    pub(super) fn session_revocation(&self, previous: Option<OwnershipId>) -> Option<Revocation> {
        let owner = previous?;
        (self.session_owner() != Some(owner)).then(|| Revocation {
            owner,
            reason: self.session_revocation_reason(),
        })
    }

    /// Why sessions of any ownership other than the current one are retired.
    pub(super) const fn session_revocation_reason(&self) -> RetirementReason {
        match self.state {
            State::Published(_) => RetirementReason::OwnerReplaced,
            State::Grace(_) => RetirementReason::OwnerDisconnected,
            State::Stopping(_) | State::Stopped(_) => RetirementReason::Stopped,
            State::Vacant | State::Reserved(_) | State::Publishing(_) | State::Fenced(_) => {
                RetirementReason::OwnerUnavailable
            }
        }
    }
}
