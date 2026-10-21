//! Session admissions, route indexes and their recovery projection have one owner.
//! The coordinator can read entries, but transitions cannot escape expiry and
//! receiver-record tracking.
use crate::{
    clock::UnixMillis,
    flow::{FlowKey, Phase as FlowPhase},
    identifiers::RelaySessionId,
    lifecycle::{ConnectionRef, ShareLifecycle},
    observation::{Journal, Observer},
    sessions::{Effects, Event, Phase, Session, SessionKey},
    share::{
        Error,
        checkpoint_tracking::{Changes, Pending},
        deadlines::{Deadlines, Key},
    },
};
use alloc::collections::BTreeMap;

#[derive(Debug)]
pub(super) struct Entry {
    pub session: Session,
    pub flows: [FlowKey; 2],
    pub deferred: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum Collection {
    Pending,
    Retained,
    Released,
}

#[derive(Debug)]
pub(super) struct Sessions {
    entries: BTreeMap<RelaySessionId, Entry>,
    routes: BTreeMap<(ConnectionRef, RelaySessionId), FlowKey>,
    last_issued: u64,
    changes: Changes,
    deadlines: Deadlines,
}

impl Sessions {
    pub const fn new(last_issued: u64) -> Self {
        Self {
            entries: BTreeMap::new(),
            routes: BTreeMap::new(),
            last_issued,
            changes: Changes::Disabled,
            deadlines: Deadlines::new(),
        }
    }

    pub const fn entries(&self) -> &BTreeMap<RelaySessionId, Entry> {
        &self.entries
    }

    pub const fn routes(&self) -> &BTreeMap<(ConnectionRef, RelaySessionId), FlowKey> {
        &self.routes
    }

    pub const fn last_issued(&self) -> u64 {
        self.last_issued
    }

    pub fn allocate(&mut self, limit: usize) -> Result<RelaySessionId, Error> {
        let admitted = self
            .entries
            .values()
            .filter(|entry| entry.session.phase() != Phase::Retired)
            .count();
        if admitted >= limit {
            return Err(Error::Capacity);
        }
        let next = self.last_issued.checked_add(1).ok_or(Error::Capacity)?;
        self.last_issued = next;
        self.changes.retention_changed();
        Ok(RelaySessionId::new(next.to_be_bytes()).expect("nonzero sequence"))
    }

    pub fn install(&mut self, session: Session, flows: &[FlowKey; 2]) {
        let key = session.key();
        for &flow in flows {
            self.routes
                .insert((flow.destination(), key.wire_id()), flow);
        }
        self.deadlines
            .set(Key::Session(key.wire_id()), session.deadline());
        self.changes.changed(key.receiver());
        self.entries.insert(
            key.wire_id(),
            Entry {
                session,
                flows: *flows,
                deferred: false,
            },
        );
    }

    pub fn handle(
        &mut self,
        id: RelaySessionId,
        lifecycle: &ShareLifecycle<Journal>,
        event: Event,
        defer_cleanup: bool,
        now: UnixMillis,
        observer: &mut impl Observer,
    ) -> Option<Effects> {
        let entry = self.entries.get_mut(&id)?;
        let before = self.changes.enabled().then(|| entry.session.snapshot());
        let effects = entry.session.handle(lifecycle, event, now, observer);
        if before.is_some_and(|snapshot| snapshot != entry.session.snapshot()) {
            // A session snapshot is encoded with its receiver's generation.
            self.changes.changed(entry.session.key().receiver());
        }
        self.deadlines
            .set(Key::Session(id), entry.session.deadline());
        if effects.retirement.is_some() {
            entry.deferred = defer_cleanup;
        }
        Some(effects)
    }

    pub fn release_cleanup(&mut self, id: RelaySessionId) -> Option<[FlowKey; 2]> {
        let entry = self.entries.get_mut(&id)?;
        entry.deferred = false;
        Some(entry.flows)
    }

    pub fn collect(
        &mut self,
        key: SessionKey,
        mut phase: impl FnMut(FlowKey) -> Option<FlowPhase>,
    ) -> Collection {
        let mut cleanup_pending = false;
        let mut retained = false;
        for destination in [key.sender(), key.receiver()] {
            let route = (destination, key.wire_id());
            if let Some(&flow) = self.routes.get(&route) {
                if let Some(phase) = phase(flow) {
                    retained = true;
                    cleanup_pending |= phase != FlowPhase::Settling;
                } else {
                    self.routes.remove(&route);
                }
            }
        }
        if cleanup_pending
            || self
                .entries
                .get(&key.wire_id())
                .is_some_and(|entry| entry.session.phase() != Phase::Retired || entry.deferred)
        {
            return Collection::Pending;
        }
        self.entries.remove(&key.wire_id());
        self.deadlines.set(Key::Session(key.wire_id()), None);
        if retained {
            Collection::Retained
        } else {
            Collection::Released
        }
    }

    pub fn restore_routes(
        &mut self,
        connection: ConnectionRef,
        flows: impl IntoIterator<Item = FlowKey>,
    ) -> Result<(), Error> {
        for flow in flows {
            if self
                .routes
                .insert((connection, flow.session().wire_id()), flow)
                .is_some()
            {
                return Err(Error::InvalidCheckpoint);
            }
        }
        Ok(())
    }

    pub fn release_destination(&mut self, connection: ConnectionRef) {
        self.routes
            .retain(|(destination, _), _| *destination != connection);
    }

    pub fn next_deadline(&self) -> Option<(UnixMillis, Key)> {
        self.deadlines.next_entry()
    }

    pub fn expire(&mut self, now: UnixMillis) -> Option<Key> {
        self.deadlines.expired(now)
    }

    pub fn enable_checkpoints(&mut self) {
        // Enabling generation tracking captures every receiver record and the manifest.
        self.changes = Changes::tracking([], false);
    }

    pub const fn checkpoint_changes(&self) -> Option<&Pending> {
        self.changes.pending()
    }

    pub fn acknowledge_checkpoints(&mut self) {
        self.changes.acknowledge();
    }
}
