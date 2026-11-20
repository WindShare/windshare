//! Hierarchical packet fair queueing. The host owns time, pacing and writer slots.
use crate::{Principal, entitlement::Class};
use alloc::collections::{BTreeMap, BTreeSet};
use core::cmp::Reverse;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Demand {
    pub principal: Principal,
    pub class: Class,
    pub bytes: u64,
}

struct Session {
    principal: Principal,
    served: u128,
    bytes: Option<u64>,
}

struct Account<K> {
    class: Class,
    served: u128,
    session_clock: u128,
    resident: usize,
    ready: BTreeSet<(u128, K)>,
}

type Ready = (Reverse<Class>, u128, Principal);

impl<K> Account<K> {
    const fn position(&self, principal: Principal) -> Ready {
        (Reverse(self.class), self.served, principal)
    }
}

/// One outstanding head per session keeps payload storage outside this module.
/// Ordered ready sets keep packet selection logarithmic as the resident set grows.
pub struct Scheduler<K> {
    sessions: BTreeMap<K, Session>,
    principals: BTreeMap<Principal, Account<K>>,
    ready: BTreeSet<Ready>,
    clocks: BTreeMap<Class, u128>,
    maximum_sessions: usize,
    maximum_frame: u64,
    pending: usize,
}

#[expect(
    clippy::missing_panics_doc,
    reason = "panics only assert private invariants, never caller preconditions"
)]
impl<K: Copy + Ord> Scheduler<K> {
    pub const fn new(maximum_sessions: usize, maximum_frame: u64) -> Self {
        Self {
            sessions: BTreeMap::new(),
            principals: BTreeMap::new(),
            ready: BTreeSet::new(),
            clocks: BTreeMap::new(),
            maximum_sessions,
            maximum_frame,
            pending: 0,
        }
    }

    pub fn request(&mut self, session: K, demand: Demand) -> bool {
        if demand.bytes == 0
            || demand.bytes > self.maximum_frame
            || (!self.sessions.contains_key(&session)
                && self.sessions.len() >= self.maximum_sessions)
            || self
                .sessions
                .get(&session)
                .is_some_and(|slot| slot.principal != demand.principal || slot.bytes.is_some())
        {
            return false;
        }
        let clock = self.clocks.get(&demand.class).copied().unwrap_or_default();
        let account = self
            .principals
            .entry(demand.principal)
            .or_insert_with(|| Account {
                class: demand.class,
                served: clock,
                session_clock: 0,
                resident: 0,
                ready: BTreeSet::new(),
            });
        self.ready.remove(&account.position(demand.principal));
        if account.class != demand.class {
            account.served = clock;
        } else if account.ready.is_empty() {
            // Idle principals cannot accumulate entitlement to future capacity.
            account.served = account.served.max(clock);
        }
        account.class = demand.class;
        let slot = self.sessions.entry(session).or_insert_with(|| {
            account.resident += 1;
            Session {
                principal: demand.principal,
                served: account.session_clock,
                bytes: None,
            }
        });
        slot.served = slot.served.max(account.session_clock);
        slot.bytes = Some(demand.bytes);
        account.ready.insert((slot.served, session));
        self.ready.insert(account.position(demand.principal));
        self.pending += 1;
        true
    }

    pub fn reclassify(&mut self, principal: Principal, class: Class) {
        if let Some(account) = self.principals.get_mut(&principal)
            && account.class != class
        {
            self.ready.remove(&account.position(principal));
            account.class = class;
            account.served = self.clocks.get(&class).copied().unwrap_or_default();
            if !account.ready.is_empty() {
                self.ready.insert(account.position(principal));
            }
        }
    }

    pub fn next(&mut self, reserve: impl FnOnce(Class, u64) -> bool) -> Option<(K, u64)> {
        // Select before testing packet size so small packets cannot starve a
        // larger head while pacing credit accumulates.
        let position = *self.ready.first()?;
        let principal = position.2;
        let account = self
            .principals
            .get_mut(&principal)
            .expect("ready principal");
        let &(served, key) = account.ready.first()?;
        let selected = self.sessions.get_mut(&key).expect("ready session");
        let bytes = selected.bytes?;
        if !reserve(account.class, bytes) {
            return None;
        }
        self.ready.remove(&position);
        account.ready.remove(&(served, key));
        selected.bytes = None;
        selected.served += u128::from(bytes);
        account.session_clock = served;
        let clock = self.clocks.entry(account.class).or_default();
        *clock = (*clock).max(account.served);
        account.served += u128::from(bytes);
        if !account.ready.is_empty() {
            self.ready.insert(account.position(principal));
        }
        self.pending -= 1;
        Some((key, bytes))
    }

    pub fn withdraw(&mut self, session: K) {
        let Some(slot) = self.sessions.get_mut(&session) else {
            return;
        };
        if slot.bytes.take().is_none() {
            return;
        }
        self.pending -= 1;
        let account = self
            .principals
            .get_mut(&slot.principal)
            .expect("session principal");
        self.ready.remove(&account.position(slot.principal));
        account.ready.remove(&(slot.served, session));
        if !account.ready.is_empty() {
            self.ready.insert(account.position(slot.principal));
        }
    }

    pub fn cancel(&mut self, session: K) {
        let Some(removed) = self.sessions.remove(&session) else {
            return;
        };
        let account = self
            .principals
            .get_mut(&removed.principal)
            .expect("session principal");
        self.ready.remove(&account.position(removed.principal));
        account.ready.remove(&(removed.served, session));
        account.resident -= 1;
        if removed.bytes.is_some() {
            self.pending -= 1;
        }
        if account.resident == 0 {
            self.principals.remove(&removed.principal);
        } else if !account.ready.is_empty() {
            self.ready.insert(account.position(removed.principal));
        }
    }

    pub const fn pending(&self) -> usize {
        self.pending
    }
}

#[cfg(test)]
#[path = "governor_tests.rs"]
mod tests;
