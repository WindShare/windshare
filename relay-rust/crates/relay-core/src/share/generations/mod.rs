//! Per-connection admission, control state and delivery ledgers.
use super::{
    AttemptLimiter, ConnectionRef, Identity, Limits,
    deadlines::{Deadlines, Key},
};
use crate::{
    clock::UnixMillis,
    flow::ConnectionLedger,
    observation::{Journal, Observer},
};
use alloc::{boxed::Box, collections::BTreeMap};

mod control;
pub(in crate::share) use control::{Connection, Phase as ConnectionPhase};
mod transitions;

/// How a detached business generation frees its admission slot after queue cleanup.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RetirementPolicy {
    /// Wait for explicit host `Input::Closed` confirming transport buffer release.
    PhysicalRelease,
    /// Release the slot as soon as unsubmitted queues are discarded (for hosts
    /// without a transport buffer-release callback, such as Cloudflare DO).
    AfterDiscard,
}

/// Active connection counts across business and control admission pools.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct ConnectionUsage {
    pub business: usize,
    pub controls: usize,
}

#[derive(Debug)]
enum Generation {
    Control {
        socket: Connection,
    },
    Business {
        socket: Connection,
        ledger: Box<ConnectionLedger<Journal>>,
    },
    Detached {
        ledger: Box<ConnectionLedger<Journal>>,
        released: bool,
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
            Self::Business { ledger, .. } | Self::Detached { ledger, .. } => Some(ledger),
            Self::Control { .. } => None,
        }
    }

    fn ledger_mut(&mut self) -> Option<&mut ConnectionLedger<Journal>> {
        match self {
            Self::Business { ledger, .. } | Self::Detached { ledger, .. } => Some(ledger),
            Self::Control { .. } => None,
        }
    }
}

#[derive(Debug)]
pub(super) struct Generations {
    identity: Identity,
    limits: Limits,
    entries: BTreeMap<ConnectionRef, Generation>,
    attempts: AttemptLimiter,
    deadlines: Deadlines,
}

impl Generations {
    pub const fn new(identity: Identity, limits: Limits, attempts: AttemptLimiter) -> Self {
        Self {
            identity,
            limits,
            entries: BTreeMap::new(),
            attempts,
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

    #[cfg(feature = "hibernation")]
    pub fn ledger_ids(&self) -> impl Iterator<Item = ConnectionRef> + '_ {
        self.entries
            .iter()
            .filter_map(|(&id, entry)| entry.ledger().map(|_| id))
    }

    #[cfg(feature = "hibernation")]
    pub fn established_ids(&self) -> impl Iterator<Item = ConnectionRef> + '_ {
        self.entries.iter().filter_map(|(&id, entry)| match entry {
            Generation::Business { socket, .. } if socket.liveness().is_some() => Some(id),
            _ => None,
        })
    }

    #[cfg(feature = "hibernation")]
    pub fn established_entry(
        &self,
        connection: ConnectionRef,
    ) -> Option<(crate::heartbeat::Liveness, &ConnectionLedger<Journal>)> {
        match self.entries.get(&connection)? {
            Generation::Business { socket, ledger } => Some((socket.liveness()?, ledger.as_ref())),
            _ => None,
        }
    }

    #[cfg(feature = "hibernation")]
    pub fn restore_established(
        &mut self,
        connection: ConnectionRef,
        liveness: crate::heartbeat::Liveness,
        ledger: ConnectionLedger<Journal>,
    ) {
        let socket = Connection::established(liveness);
        socket.synchronize_deadlines(connection, &mut self.deadlines);
        self.deadlines
            .set(Key::Ledger(connection), ledger.deadline());
        self.entries.insert(
            connection,
            Generation::Business {
                socket,
                ledger: Box::new(ledger),
            },
        );
    }

    #[cfg(test)]
    pub fn control_deadline(&self, connection: &ConnectionRef) -> Option<UnixMillis> {
        self.deadlines.get(Key::Control(*connection))
    }

    #[cfg(test)]
    pub fn is_detached(&self, connection: &ConnectionRef) -> bool {
        matches!(
            self.entries.get(connection),
            Some(Generation::Detached { .. })
        )
    }

    pub fn usage(&self) -> ConnectionUsage {
        let mut usage = ConnectionUsage::default();
        for entry in self.entries.values() {
            match entry {
                Generation::Control { .. } => usage.controls += 1,
                Generation::Business { .. } | Generation::Detached { .. } => usage.business += 1,
            }
        }
        usage
    }

    pub fn released(&self) -> impl Iterator<Item = ConnectionRef> + '_ {
        self.entries.iter().filter_map(|(&id, entry)| {
            matches!(entry, Generation::Detached { released: true, .. }).then_some(id)
        })
    }

    pub fn detached_ids(&self) -> impl Iterator<Item = ConnectionRef> + '_ {
        self.entries
            .iter()
            .filter_map(|(&id, entry)| matches!(entry, Generation::Detached { .. }).then_some(id))
    }

    pub fn has_detached(&self) -> bool {
        self.entries
            .values()
            .any(|entry| matches!(entry, Generation::Detached { .. }))
    }

    #[cfg(feature = "hibernation")]
    pub fn connected_count(&self) -> usize {
        self.entries
            .values()
            .filter(|entry| entry.socket().is_some())
            .count()
    }

    #[cfg(feature = "hibernation")]
    pub fn retained_count(&self) -> usize {
        self.entries
            .values()
            .filter(|entry| !matches!(entry, Generation::Control { .. }))
            .count()
    }

    pub fn unsettled_count(&self) -> usize {
        self.entries
            .values()
            .filter(|entry| {
                entry
                    .ledger()
                    .is_some_and(|ledger| ledger.metadata_usage().retained_flows != 0)
            })
            .count()
    }

    pub fn next_deadline(&self) -> Option<(UnixMillis, Key)> {
        self.deadlines.next_entry()
    }

    pub fn expire(&mut self, now: UnixMillis) -> Option<Key> {
        self.deadlines.expired(now)
    }

    /// Every exact-accounting mutation also refreshes its deadline index.
    pub fn update_ledger<R>(
        &mut self,
        connection: ConnectionRef,
        change: impl FnOnce(&mut ConnectionLedger<Journal>) -> R,
        observer: &mut impl Observer,
    ) -> Option<R> {
        let entry = self.entries.get_mut(&connection)?;
        let ledger = entry.ledger_mut()?;
        let result = change(ledger);
        for observation in ledger.observer_mut().0.drain(..) {
            observer.observe(observation);
        }
        self.deadlines
            .set(Key::Ledger(connection), ledger.deadline());
        Some(result)
    }
}
