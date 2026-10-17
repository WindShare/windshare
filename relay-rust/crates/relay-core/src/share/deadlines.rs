use crate::{clock::UnixMillis, identifiers::RelaySessionId, lifecycle::ConnectionRef};
use alloc::collections::{BTreeMap, BTreeSet};

#[derive(Clone, Copy, Debug, Eq, PartialEq, Ord, PartialOrd)]
pub(super) enum Key {
    Lifecycle,
    Handshake(ConnectionRef),
    Liveness(ConnectionRef),
    Session(RelaySessionId),
    Ledger(ConnectionRef),
    Retirement(ConnectionRef),
}

// Updating one connection must not scan every session on each forwarded frame.
#[derive(Debug, Default)]
pub(super) struct Deadlines {
    by_key: BTreeMap<Key, u64>,
    ordered: BTreeSet<(u64, Key)>,
}
impl Deadlines {
    pub fn set(&mut self, key: Key, deadline: Option<UnixMillis>) {
        if self.by_key.get(&key).copied() == deadline.map(|at| at.0) {
            return;
        }
        if let Some(old) = self.by_key.remove(&key) {
            self.ordered.remove(&(old, key));
        }
        if let Some(at) = deadline {
            self.by_key.insert(key, at.0);
            self.ordered.insert((at.0, key));
        }
    }
    pub fn next(&self) -> Option<UnixMillis> {
        self.ordered.first().map(|(at, _)| UnixMillis(*at))
    }
    pub fn expired(&mut self, now: UnixMillis) -> Option<Key> {
        let &(at, key) = self.ordered.first()?;
        if at > now.0 {
            return None;
        }
        self.ordered.pop_first();
        self.by_key.remove(&key);
        Some(key)
    }
}
