//! Rebuilding one share's actor from storage and whatever outlived the last one.
//!
//! An actor rebuilt over surviving sockets, such as after Cloudflare
//! hibernation, keeps the stored ownership and reinstalls its sessions, so
//! receivers keep forwarding. Only state that the record, the surviving
//! connections and sibling snapshots all agree on becomes live again.

use crate::{
    clock::UnixMillis,
    join::SESSION_ADMISSION_TIMEOUT,
    sessions::{
        EndReason, Retirement, RetirementReason, Session, SessionKey, SessionSnapshot,
        SnapshotFault,
    },
};

use super::{
    Action, ConnectionRef, Decision, GraceRoute, ObservedEvent, Observer, OperationId,
    PublishedRegistration, Record, SENDER_GRACE, ShareLifecycle, State, StopWork,
};

/// What a restore rebuilds one share's actor from.
///
/// Storage always survives. Hibernation also keeps sockets and the session
/// snapshots hosts record beside them; a process restart keeps neither.
#[derive(Clone, Copy, Debug)]
pub struct Restoration<'s> {
    /// What storage holds for the share; `None` only when no record exists.
    pub record: Option<Record>,
    /// Connections whose exact lifetimes outlived the previous actor. Restore
    /// trusts them to re-adopt the stored owner and session receivers.
    pub connections: &'s [ConnectionRef],
    /// The latest `Session::snapshot` recorded for each live session.
    pub sessions: &'s [SessionSnapshot],
}

impl Restoration<'static> {
    /// After a process restart only storage survives.
    #[must_use]
    pub const fn restarted(record: Option<Record>) -> Self {
        Self {
            record,
            connections: &[],
            sessions: &[],
        }
    }
}

/// Restore's decision for one surviving session snapshot.
#[derive(Debug, Eq, PartialEq)]
pub enum SessionRestore {
    /// The same admission, with its permits and original deadline. Store it
    /// where the snapshot was recorded.
    Restored(Session),
    /// The snapshot never becomes live again. Drop it, then clean up exactly
    /// as for any other retirement, including delivery occupancy it holds.
    Retired(Retirement),
}

impl<O: Observer> ShareLifecycle<O> {
    /// Installs the stored state and returns the host action it requires,
    /// with the operation and connection the restore observation names.
    pub(super) fn install(
        &mut self,
        stored: Option<Record>,
        connections: &[ConnectionRef],
        now: UnixMillis,
    ) -> (Action<'static>, Option<OperationId>, Option<ConnectionRef>) {
        match stored {
            None => (Action::None, None, None),
            Some(Record::Unresolved(unresolved)) => {
                self.state = State::Fenced(unresolved);
                let id = unresolved.id;
                (Action::Reconcile(id), Some(id), Some(id.connection))
            }
            Some(Record::Published(owned)) => {
                // A surviving owner keeps the stored ownership: rebuilding the
                // actor replaces no connection and retires no session.
                if connections.contains(&owned.owner) {
                    self.state = State::Published(PublishedRegistration {
                        owner: owned.ownership(),
                        route: owned.route,
                    });
                    return (Action::None, None, Some(owned.owner));
                }
                let route = GraceRoute::new(
                    owned.route,
                    owned.ownership(),
                    now.saturating_add(SENDER_GRACE),
                );
                self.state = State::Grace(route);
                (Action::Detached(route), None, Some(owned.owner))
            }
            Some(Record::Grace(route)) => {
                self.state = State::Grace(route);
                (Action::None, None, None)
            }
            Some(Record::Stopping(pending)) => {
                // The durable obligation survives hibernation and process
                // restart. Repeating exact cleanup is safe; re-adoption is not.
                let work = StopWork::Retire(pending);
                self.state = State::Stopping(work);
                (
                    Action::Stop(work),
                    Some(pending.id()),
                    Some(pending.id().connection()),
                )
            }
            Some(Record::Stopped(stopped)) => {
                self.state = State::Stopped(stopped);
                (Action::None, None, None)
            }
        }
    }

    /// Decides every surviving snapshot, in order, before any event can
    /// change the installed state they are checked against.
    pub(super) fn restore_sessions(
        &mut self,
        restoration: &Restoration<'_>,
        now: UnixMillis,
        mut install: impl FnMut(usize, SessionRestore),
    ) {
        for (index, &snapshot) in restoration.sessions.iter().enumerate() {
            let key = snapshot.key();
            let (outcome, decision) = match self.check_snapshot(index, snapshot, restoration, now) {
                Ok(()) => (
                    SessionRestore::Restored(Session::restored(snapshot)),
                    Decision::SessionRestored(key),
                ),
                Err(reason) => {
                    let retirement = Retirement {
                        session: key,
                        reason,
                    };
                    (
                        SessionRestore::Retired(retirement),
                        Decision::SessionRetired(retirement),
                    )
                }
            };
            let phase = self.phase();
            self.observe(
                now,
                ObservedEvent::RestoreSession(index),
                Some(key.admission()),
                Some(key.receiver()),
                phase,
                decision,
            );
            install(index, outcome);
        }
    }

    fn check_snapshot(
        &self,
        index: usize,
        snapshot: SessionSnapshot,
        restoration: &Restoration<'_>,
        now: UnixMillis,
    ) -> Result<(), RetirementReason> {
        let key = snapshot.key();
        if let Some(fault) = self.snapshot_fault(index, key, restoration.sessions) {
            return Err(RetirementReason::InvalidSnapshot(fault));
        }
        // A lost owner already moved the route to grace, so this also checks
        // that the sender survived.
        let published = match self.state {
            State::Published(published) if published.owner == key.owner() => published,
            _ => return Err(self.session_revocation_reason()),
        };
        if key.instance() != published.route.registration().share_instance() {
            return Err(RetirementReason::InvalidSnapshot(
                SnapshotFault::InstanceMismatch,
            ));
        }
        if !restoration.connections.contains(&key.receiver()) {
            return Err(RetirementReason::Ended(EndReason::ReceiverDisconnected));
        }
        match snapshot.deadline() {
            Some(deadline) if now >= deadline => Err(RetirementReason::AdmissionExpired),
            // JOIN set the deadline before now, so a later one would extend
            // the admission budget instead of resuming it.
            Some(deadline) if deadline > now.saturating_add(SESSION_ADMISSION_TIMEOUT) => Err(
                RetirementReason::InvalidSnapshot(SnapshotFault::DeadlineBeyondBudget),
            ),
            _ => Ok(()),
        }
    }

    /// Faults no admission of this share can have, whatever state survived.
    fn snapshot_fault(
        &self,
        index: usize,
        key: SessionKey,
        sessions: &[SessionSnapshot],
    ) -> Option<SnapshotFault> {
        if key.relay() != self.relay || key.share() != self.share {
            Some(SnapshotFault::ForeignLifecycle)
        } else if key.admitted_by() == self.incarnation {
            Some(SnapshotFault::UnissuedAdmission)
        } else if key.receiver() == key.sender() {
            Some(SnapshotFault::InvalidReceiver)
        } else if sessions
            .iter()
            .enumerate()
            .any(|(other, sibling)| other != index && sibling.key().collides(key))
        {
            // Neither claimant can be trusted to own the routing ID.
            Some(SnapshotFault::Duplicate)
        } else {
            None
        }
    }
}
