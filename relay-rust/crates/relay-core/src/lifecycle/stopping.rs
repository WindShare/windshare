//! Irreversible STOP transactions and their durable cleanup obligations.

use crate::{
    identifiers::{ShareInstance, ShareRoute, StopId},
    registration::Registration,
    stop::{StopAuthority, StopInit, Stopped},
};

use super::{
    Action, ConnectionRef, Observer, OperationId, Record, Refusal, ShareLifecycle, State,
    StoppedShare,
};

/// Exact host resources fenced by STOP. Cleanup must never resolve a newer
/// connection by slot ID or remove the share's permanent stop evidence.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum StopCleanup {
    /// No registration resources exist, including a grace already detached.
    None,
    /// An unauthenticated handshake and its exact connection resources.
    Reservation(OperationId),
    /// An in-flight or fenced publication: quiesce its writes and remove its
    /// descriptor/prepared resources without deleting the STOP record.
    Publication(OperationId),
    /// The published owner's exact connection lifetime and its relay sessions.
    Owner(ConnectionRef),
}

impl StopCleanup {
    pub(super) const fn operation(self) -> Option<OperationId> {
        match self {
            Self::Reservation(id) | Self::Publication(id) => Some(id),
            Self::None | Self::Owner(_) => None,
        }
    }
}

/// The irrevocable stop fact and outstanding cleanup stored together, so a
/// restart cannot confuse a durable STOP with completed participant retirement.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PendingStop {
    pub(super) id: OperationId,
    // Relay identity comes from the operation; storing it twice would make
    // every lifecycle state and observation copy needlessly large.
    pub(super) route: ShareRoute,
    pub(super) share_instance: ShareInstance,
    pub(super) stop_id: StopId,
    pub(super) cleanup: StopCleanup,
}

impl PendingStop {
    pub(super) fn new(id: OperationId, stopped: StoppedShare, cleanup: StopCleanup) -> Self {
        debug_assert_eq!(id.relay(), stopped.relay);
        debug_assert_eq!(id.share(), stopped.route.share_id());
        Self {
            id,
            route: stopped.route,
            share_instance: stopped.share_instance,
            stop_id: stopped.stop_id,
            cleanup,
        }
    }

    /// Actor-issued transaction identity and the requesting connection lifetime.
    #[must_use]
    pub const fn id(self) -> OperationId {
        self.id
    }

    /// The permanent fact that survives after cleanup.
    #[must_use]
    pub const fn stopped(self) -> StoppedShare {
        StoppedShare {
            relay: self.id.relay(),
            route: self.route,
            share_instance: self.share_instance,
            stop_id: self.stop_id,
        }
    }

    /// Resources whose retirement must be confirmed before STOPPED.
    #[must_use]
    pub const fn cleanup(self) -> StopCleanup {
        self.cleanup
    }

    /// The record stored before participant cleanup, retained on any failure.
    #[must_use]
    pub const fn record(self) -> Record {
        Record::Stopping(self)
    }
}

/// Phase identity carried through asynchronous host work and observations.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum StopStep {
    /// Durably establish the stop fact and its cleanup obligation.
    Persist,
    /// Finish exact participant cleanup and quiesce outstanding writes.
    Retire,
    /// Durably record that cleanup is complete.
    Commit,
}

/// One idempotent host obligation in an irreversible STOP.
///
/// Hosts serialize durable record changes per share.
/// Publication writes are conditional on absence/their exact unresolved record;
/// rollback and expiry delete only their own records. Neither may replace or
/// delete a stopping/stopped record, even if a callback arrives late.
///
/// Every step is idempotent. Failure or an ambiguous result is retried with the
/// same identity, with host backoff; it never returns the share to service.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum StopWork {
    /// Immediately hide the route and revoke its routing authority. Atomically
    /// replace its record with `pending.record()`, preserving the exact cleanup
    /// obligation. Do not downgrade an identical final stopped record.
    Persist(PendingStop),
    /// Retire `pending.cleanup()` and its relay sessions, discard unsent frames
    /// and descriptor bytes, and quiesce all outstanding publication work.
    /// Complete only when exact resources and writes are gone. Keep stop evidence.
    Retire(PendingStop),
    /// Replace only this pending record with `pending.stopped().record()`.
    /// An identical final record also confirms completion. Only confirmed
    /// completion of this step authorizes STOPPED.
    Commit(PendingStop),
}

impl StopWork {
    /// The durable transaction this step belongs to.
    #[must_use]
    pub const fn pending(self) -> PendingStop {
        match self {
            Self::Persist(pending) | Self::Retire(pending) | Self::Commit(pending) => pending,
        }
    }

    /// The phase whose completion a callback reports.
    #[must_use]
    pub const fn step(self) -> StopStep {
        match self {
            Self::Persist(_) => StopStep::Persist,
            Self::Retire(_) => StopStep::Retire,
            Self::Commit(_) => StopStep::Commit,
        }
    }
}

/// Evidence for one STOP step, not for the whole transaction.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum StopResult {
    /// The requested durable write or exact cleanup completed.
    Complete,
    /// Failed or ambiguous work: retain the fence and retry this same step.
    Uncertain,
}

/// Permission to acknowledge one authenticated STOP on one connection lifetime.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct StopAcknowledgement {
    connection: ConnectionRef,
    stopped: Stopped,
}

impl StopAcknowledgement {
    /// The original or retrying STOP connection; never a replacement generation.
    #[must_use]
    pub const fn connection(self) -> ConnectionRef {
        self.connection
    }

    /// The STOPPED frame, available only after durability and cleanup.
    #[must_use]
    pub const fn acknowledgement(self) -> Stopped {
        self.stopped
    }
}

impl<O: Observer> ShareLifecycle<O> {
    #[expect(
        clippy::needless_pass_by_value,
        reason = "consuming STOP authority prevents unauthenticated retirement"
    )]
    pub(super) fn stop(
        &mut self,
        connection: ConnectionRef,
        authority: StopAuthority,
    ) -> Action<'static> {
        let init = authority.init();
        if init.relay() != self.relay {
            return Action::Refused(Refusal::WrongRelay);
        }
        if init.route().share_id() != self.share {
            return Action::Refused(Refusal::WrongShare);
        }
        match self.state {
            State::Stopping(work) => {
                return Action::Refused(
                    match_stop(work.pending().stopped(), init)
                        .err()
                        .unwrap_or(Refusal::StopPending),
                );
            }
            State::Stopped(stopped) => {
                return match match_stop(stopped, init) {
                    Ok(()) => acknowledge(connection, stopped),
                    Err(reason) => Action::Refused(reason),
                };
            }
            _ => {}
        }
        let cleanup = match self.stop_cleanup(init) {
            Ok(cleanup) => cleanup,
            Err(reason) => return Action::Refused(reason),
        };
        let Some(sequence) = self.next_sequence() else {
            return Action::Refused(Refusal::SequenceExhausted);
        };
        let pending = PendingStop::new(
            self.operation(connection, sequence),
            StoppedShare {
                relay: self.relay,
                route: init.route(),
                share_instance: init.share_instance(),
                stop_id: init.stop_id(),
            },
            cleanup,
        );
        let work = StopWork::Persist(pending);
        self.state = State::Stopping(work);
        Action::Stop(work)
    }

    fn stop_cleanup(&self, init: &StopInit) -> Result<StopCleanup, Refusal> {
        let (registration, cleanup) = match self.state {
            State::Vacant => return Ok(StopCleanup::None),
            // A reservation is not authenticated ownership. An attacker naming
            // another instance must not prevent the sender's valid STOP.
            State::Reserved(pending) => return Ok(StopCleanup::Reservation(pending.id)),
            State::Publishing(pending) => (
                *pending.init.registration(),
                StopCleanup::Publication(pending.id),
            ),
            State::Fenced(unresolved) => (
                unresolved.registration,
                StopCleanup::Publication(unresolved.id),
            ),
            State::Published(published) => (
                published.route.registration,
                StopCleanup::Owner(published.owner.connection),
            ),
            State::Activating(commit) => (
                commit.route.route.registration,
                StopCleanup::Owner(commit.route.owner),
            ),
            State::Grace(route) => (route.route.registration, StopCleanup::None),
            State::Stopping(_) | State::Stopped(_) => unreachable!(),
        };
        match_registration(registration, init)?;
        Ok(cleanup)
    }

    pub(super) fn finish_stop(&mut self, work: StopWork, result: StopResult) -> Action<'static> {
        if !matches!(self.state, State::Stopping(current) if current == work) {
            return Action::Refused(Refusal::StaleOperation);
        }
        if result == StopResult::Uncertain {
            return Action::Stop(work);
        }
        let next = match work {
            StopWork::Persist(pending) => StopWork::Retire(pending),
            StopWork::Retire(pending) => StopWork::Commit(pending),
            StopWork::Commit(pending) => {
                self.state = State::Stopped(pending.stopped());
                return acknowledge(pending.id.connection, pending.stopped());
            }
        };
        self.state = State::Stopping(next);
        Action::Stop(next)
    }
}

fn match_registration(registration: Registration, init: &StopInit) -> Result<(), Refusal> {
    match_instance(registration.route(), registration.share_instance(), init)
}

fn match_instance(
    route: ShareRoute,
    instance: ShareInstance,
    init: &StopInit,
) -> Result<(), Refusal> {
    if route == init.route() && instance == init.share_instance() {
        Ok(())
    } else {
        Err(Refusal::StopMismatch)
    }
}

fn match_stop(stopped: StoppedShare, init: &StopInit) -> Result<(), Refusal> {
    match_instance(stopped.route, stopped.share_instance, init)?;
    if stopped.stop_id == init.stop_id() {
        Ok(())
    } else {
        Err(Refusal::Stopped)
    }
}

const fn acknowledge(connection: ConnectionRef, stopped: StoppedShare) -> Action<'static> {
    Action::Stopped(StopAcknowledgement {
        connection,
        stopped: Stopped::new(stopped.stop_id),
    })
}
