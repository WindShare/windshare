//! A generation owns its admission and delivery responsibility even after its
//! socket disappears. Exact records and conservative reservations are exclusive;
//! secondary indexes never decide whether a generation still exists.
use super::{
    AttemptLimiter, ConnectionRef, Identity, Limits,
    checkpoint_tracking::Changes,
    deadlines::{Deadlines, Key},
};
use crate::{
    clock::UnixMillis,
    flow::ConnectionLedger,
    observation::{Journal, Observer},
};
use alloc::{boxed::Box, collections::BTreeMap};

mod checkpoint;
mod control;
pub(in crate::share) use control::{Connection, Phase as ConnectionPhase};
mod retirement;
mod transitions;

pub use retirement::{ConnectionUsage, RetirementPolicy};
pub(super) use retirement::{Detached, RetirementProgress};

// Control ingress stays small; delivery storage is allocated only on business admission.
#[derive(Debug)]
enum Delivery {
    Exact(Box<ConnectionLedger<Journal>>),
    Unavailable(crate::flow::Counts),
}

#[derive(Debug)]
enum Generation {
    Control {
        socket: Connection,
        deadline: UnixMillis,
    },
    Business {
        socket: Connection,
        ledger: Box<ConnectionLedger<Journal>>,
    },
    Detached {
        delivery: Delivery,
        state: Detached,
    },
}

impl Generation {
    const fn socket(&self) -> Option<&Connection> {
        match self {
            Self::Control { socket, .. } | Self::Business { socket, .. } => Some(socket),
            Self::Detached { .. } => None,
        }
    }

    const fn socket_mut(&mut self) -> Option<&mut Connection> {
        match self {
            Self::Control { socket, .. } | Self::Business { socket, .. } => Some(socket),
            Self::Detached { .. } => None,
        }
    }

    fn ledger(&self) -> Option<&ConnectionLedger<Journal>> {
        match self {
            Self::Business { ledger, .. }
            | Self::Detached {
                delivery: Delivery::Exact(ledger),
                ..
            } => Some(ledger),
            Self::Control { .. }
            | Self::Detached {
                delivery: Delivery::Unavailable(_),
                ..
            } => None,
        }
    }

    fn ledger_mut(&mut self) -> Option<&mut ConnectionLedger<Journal>> {
        match self {
            Self::Business { ledger, .. }
            | Self::Detached {
                delivery: Delivery::Exact(ledger),
                ..
            } => Some(ledger),
            Self::Control { .. }
            | Self::Detached {
                delivery: Delivery::Unavailable(_),
                ..
            } => None,
        }
    }
}

#[derive(Debug)]
pub(super) struct Generations {
    identity: Identity,
    limits: Limits,
    entries: BTreeMap<ConnectionRef, Generation>,
    attempts: AttemptLimiter,
    changes: Changes,
    deadlines: Deadlines,
}

impl Generations {
    pub const fn new(identity: Identity, limits: Limits, attempts: AttemptLimiter) -> Self {
        Self {
            identity,
            limits,
            entries: BTreeMap::new(),
            attempts,
            changes: Changes::Disabled,
            deadlines: Deadlines::new(),
        }
    }

    pub fn contains(&self, connection: &ConnectionRef) -> bool {
        self.entries.contains_key(connection)
    }

    pub fn connection(&self, connection: &ConnectionRef) -> Option<&Connection> {
        self.entries.get(connection)?.socket()
    }

    pub fn ledger(&self, connection: &ConnectionRef) -> Option<&ConnectionLedger<Journal>> {
        self.entries.get(connection)?.ledger()
    }

    pub fn ledgers(&self) -> impl Iterator<Item = &ConnectionLedger<Journal>> {
        self.entries.values().filter_map(Generation::ledger)
    }

    pub fn ledger_ids(&self) -> impl Iterator<Item = ConnectionRef> + '_ {
        self.entries
            .iter()
            .filter_map(|(&id, entry)| entry.ledger().map(|_| id))
    }

    pub fn record_ids(&self) -> impl Iterator<Item = ConnectionRef> + '_ {
        self.entries.iter().filter_map(|(&id, entry)| {
            (!matches!(
                entry,
                Generation::Detached {
                    delivery: Delivery::Unavailable(_),
                    ..
                }
            ))
            .then_some(id)
        })
    }

    pub fn control_deadline(&self, connection: &ConnectionRef) -> Option<UnixMillis> {
        match self.entries.get(connection)? {
            Generation::Control { deadline, .. } => Some(*deadline),
            _ => None,
        }
    }

    pub fn connected_count(&self) -> usize {
        self.entries
            .values()
            .filter(|entry| entry.socket().is_some())
            .count()
    }

    pub fn retained_count(&self) -> usize {
        self.entries
            .values()
            .filter(|entry| !matches!(entry, Generation::Control { .. }))
            .count()
    }

    pub fn unsettled_count(&self) -> usize {
        self.entries
            .values()
            .filter(|entry| match entry {
                Generation::Detached {
                    delivery: Delivery::Unavailable(bound),
                    ..
                } => *bound != crate::flow::Counts::ZERO,
                _ => entry
                    .ledger()
                    .is_some_and(|ledger| ledger.metadata_usage().retained_flows != 0),
            })
            .count()
    }

    pub fn enable_checkpoints(&mut self) {
        for entry in self.entries.values_mut() {
            if let Some(ledger) = entry.ledger_mut() {
                let _ = ledger.take_checkpoint_change();
            }
        }
        self.changes = Changes::tracking(self.record_ids(), true);
    }

    pub const fn checkpoint_changes(&self) -> Option<&super::checkpoint_tracking::Pending> {
        self.changes.pending()
    }

    pub fn acknowledge_checkpoints(&mut self) {
        self.changes.acknowledge();
    }

    pub const fn attempts(&self) -> super::admission::AttemptState {
        self.attempts.state()
    }

    pub fn has_record(&self, connection: &ConnectionRef) -> bool {
        self.entries.get(connection).is_some_and(|entry| {
            !matches!(
                entry,
                Generation::Detached {
                    delivery: Delivery::Unavailable(_),
                    ..
                }
            )
        })
    }

    fn schedule_retirement(&mut self, connection: ConnectionRef) {
        let deadline = self
            .retirement_state(&connection)
            .and_then(Detached::deadline);
        self.deadlines.set(Key::Retirement(connection), deadline);
    }

    pub fn next_deadline(&self) -> Option<(UnixMillis, Key)> {
        self.deadlines.next_entry()
    }

    pub fn expire(&mut self, now: UnixMillis) -> Option<Key> {
        self.deadlines.expired(now)
    }

    /// Every exact-accounting mutation also updates its recovery projection and
    /// deadline. Callers cannot forget the retired generation's manifest charge.
    pub fn update_ledger<R>(
        &mut self,
        connection: ConnectionRef,
        change: impl FnOnce(&mut ConnectionLedger<Journal>) -> R,
        observer: &mut impl Observer,
    ) -> Option<R> {
        let entry = self.entries.get_mut(&connection)?;
        let retired = matches!(
            entry,
            Generation::Detached {
                state: Detached::RiskRetained { .. },
                ..
            }
        );
        let ledger = entry.ledger_mut()?;
        let result = change(ledger);
        if self.changes.enabled() && ledger.take_checkpoint_change() {
            self.changes.changed(connection);
            if retired {
                self.changes.retention_changed();
            }
        }
        for observation in ledger.observer_mut().0.drain(..) {
            observer.observe(observation);
        }
        self.deadlines
            .set(Key::Ledger(connection), ledger.deadline());
        Some(result)
    }
}
