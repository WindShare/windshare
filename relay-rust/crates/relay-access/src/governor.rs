//! Two-tier hierarchical fair queueing. The host owns time, pacing and writer slots.
//!
//! Owner traffic strictly preempts public traffic. Public classes share capacity
//! through class-level weighted fair queueing, then distribute each class's share
//! fairly across principals and sessions.
use crate::{Principal, entitlement::Class};
use alloc::collections::{BTreeMap, BTreeSet};
use core::cmp::Reverse;

/// Relative virtual-time cost per forwarded byte for public classes.
///
/// A 19:1 ratio keeps ~95% of contended public capacity for `Standard` while
/// leaving ~5% for `Lowest` so small E2E handshakes, browsing and P2P signaling
/// pass promptly without starving or enabling per-session Sybil amplification.
pub const STANDARD_WEIGHT_COST: u128 = 1;
pub const LOWEST_WEIGHT_COST: u128 = 19;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Demand {
    pub principal: Principal,
    pub class: Class,
    pub bytes: u64,
}

#[derive(Clone, Copy)]
enum PublicClass {
    Standard,
    Lowest,
}

impl PublicClass {
    const ALL: [Self; 2] = [Self::Standard, Self::Lowest];

    const fn from_class(class: Class) -> Option<Self> {
        match class {
            Class::Owner => None,
            Class::Standard => Some(Self::Standard),
            Class::Lowest => Some(Self::Lowest),
        }
    }

    const fn class(self) -> Class {
        match self {
            Self::Standard => Class::Standard,
            Self::Lowest => Class::Lowest,
        }
    }

    const fn weight_cost(self) -> u128 {
        match self {
            Self::Standard => STANDARD_WEIGHT_COST,
            Self::Lowest => LOWEST_WEIGHT_COST,
        }
    }
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

struct ClassQueue {
    served: u128,
    principal_clock: u128,
    ready: BTreeSet<(u128, Principal)>,
}

impl ClassQueue {
    const fn new() -> Self {
        Self {
            served: 0,
            principal_clock: 0,
            ready: BTreeSet::new(),
        }
    }
}

struct Candidate<K> {
    class: Class,
    principal: Principal,
    principal_served: u128,
    session_served: u128,
    session: K,
    bytes: u64,
}

/// One outstanding head per session keeps payload storage outside this module.
/// Ordered ready sets keep packet selection logarithmic as the resident set grows.
pub struct Scheduler<K> {
    sessions: BTreeMap<K, Session>,
    principals: BTreeMap<Principal, Account<K>>,
    owner: ClassQueue,
    standard: ClassQueue,
    lowest: ClassQueue,
    public_clock: u128,
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
            owner: ClassQueue::new(),
            standard: ClassQueue::new(),
            lowest: ClassQueue::new(),
            public_clock: 0,
            maximum_sessions,
            maximum_frame,
            pending: 0,
        }
    }

    const fn queue(&self, class: Class) -> &ClassQueue {
        match class {
            Class::Owner => &self.owner,
            Class::Standard => &self.standard,
            Class::Lowest => &self.lowest,
        }
    }

    const fn queue_mut(&mut self, class: Class) -> &mut ClassQueue {
        match class {
            Class::Owner => &mut self.owner,
            Class::Standard => &mut self.standard,
            Class::Lowest => &mut self.lowest,
        }
    }

    fn activate_principal(&mut self, class: Class, served: u128, principal: Principal) {
        let public_clock = self.public_clock;
        let queue = self.queue_mut(class);
        if PublicClass::from_class(class).is_some() && queue.ready.is_empty() {
            // An idle public class cannot accumulate entitlement to future capacity.
            queue.served = queue.served.max(public_clock);
        }
        queue.ready.insert((served, principal));
    }

    fn peek_class(&self, class: Class) -> Option<Candidate<K>> {
        let &(principal_served, principal) = self.queue(class).ready.first()?;
        let account = self.principals.get(&principal).expect("ready principal");
        let &(session_served, session) = account.ready.first()?;
        let selected = self.sessions.get(&session).expect("ready session");
        let bytes = selected.bytes?;
        Some(Candidate {
            class,
            principal,
            principal_served,
            session_served,
            session,
            bytes,
        })
    }

    fn select(&self) -> Option<Candidate<K>> {
        if let Some(owner) = self.peek_class(Class::Owner) {
            return Some(owner);
        }
        PublicClass::ALL
            .into_iter()
            .filter_map(|public| {
                let candidate = self.peek_class(public.class())?;
                let cost = u128::from(candidate.bytes).saturating_mul(public.weight_cost());
                let finish = self.queue(public.class()).served.saturating_add(cost);
                Some((finish, Reverse(public.class()), candidate))
            })
            .min_by_key(|&(finish, priority, _)| (finish, priority))
            .map(|(_, _, candidate)| candidate)
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
        let target_clock = self.queue(demand.class).principal_clock;
        let previous = self
            .principals
            .get(&demand.principal)
            .map(|account| (account.class, account.served, !account.ready.is_empty()));
        let account = self
            .principals
            .entry(demand.principal)
            .or_insert_with(|| Account {
                class: demand.class,
                served: target_clock,
                session_clock: 0,
                resident: 0,
                ready: BTreeSet::new(),
            });
        let class_changed = account.class != demand.class;
        if class_changed {
            account.class = demand.class;
            account.served = target_clock;
        } else if account.ready.is_empty() {
            // Idle principals cannot accumulate entitlement to future capacity.
            account.served = account.served.max(target_clock);
        }
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
        let account_served = account.served;

        match previous {
            Some((old_class, old_served, true)) if class_changed => {
                self.queue_mut(old_class)
                    .ready
                    .remove(&(old_served, demand.principal));
                self.activate_principal(demand.class, account_served, demand.principal);
            }
            Some((_, _, true)) => {}
            _ => self.activate_principal(demand.class, account_served, demand.principal),
        }
        self.pending += 1;
        true
    }

    pub fn reclassify(&mut self, principal: Principal, class: Class) -> Option<Class> {
        let target_clock = self.queue(class).principal_clock;
        let account = self.principals.get_mut(&principal)?;
        if account.class == class {
            return None;
        }
        let previous = account.class;
        let previous_served = account.served;
        let was_ready = !account.ready.is_empty();
        account.class = class;
        account.served = target_clock;
        if was_ready {
            self.queue_mut(previous)
                .ready
                .remove(&(previous_served, principal));
            self.activate_principal(class, target_clock, principal);
        }
        Some(previous)
    }

    pub fn next(&mut self, reserve: impl FnOnce(Class, u64) -> bool) -> Option<(K, u64)> {
        // Select before testing packet size so small packets cannot starve a
        // larger head while pacing credit accumulates.
        let candidate = self.select()?;
        if !reserve(candidate.class, candidate.bytes) {
            return None;
        }
        let account = self
            .principals
            .get_mut(&candidate.principal)
            .expect("ready principal");
        let selected = self
            .sessions
            .get_mut(&candidate.session)
            .expect("ready session");
        account
            .ready
            .remove(&(candidate.session_served, candidate.session));
        selected.bytes = None;
        selected.served = selected.served.saturating_add(u128::from(candidate.bytes));
        account.session_clock = candidate.session_served;
        account.served = account.served.saturating_add(u128::from(candidate.bytes));
        let account_served = account.served;
        let account_still_ready = !account.ready.is_empty();

        let queue = self.queue_mut(candidate.class);
        queue
            .ready
            .remove(&(candidate.principal_served, candidate.principal));
        queue.principal_clock = queue.principal_clock.max(candidate.principal_served);
        if account_still_ready {
            queue.ready.insert((account_served, candidate.principal));
        }

        if let Some(public) = PublicClass::from_class(candidate.class) {
            let start = queue.served;
            queue.served = start
                .saturating_add(u128::from(candidate.bytes).saturating_mul(public.weight_cost()));
            // Advance shared virtual time by the minimum-cost frame unit, not by
            // Lowest's multiplied penalty, so higher public classes keep their
            // weighted lead across brief single-session actor wake gaps.
            self.public_clock = self
                .public_clock
                .max(start.saturating_add(u128::from(candidate.bytes)));
        }

        self.pending -= 1;
        Some((candidate.session, candidate.bytes))
    }

    pub fn withdraw(&mut self, session: K) {
        let Some(slot) = self.sessions.get_mut(&session) else {
            return;
        };
        if slot.bytes.take().is_none() {
            return;
        }
        self.pending -= 1;
        let principal = slot.principal;
        let account = self
            .principals
            .get_mut(&principal)
            .expect("session principal");
        account.ready.remove(&(slot.served, session));
        if account.ready.is_empty() {
            let class = account.class;
            let served = account.served;
            self.queue_mut(class).ready.remove(&(served, principal));
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
        let was_ready = !account.ready.is_empty();
        account.ready.remove(&(removed.served, session));
        account.resident -= 1;
        if removed.bytes.is_some() {
            self.pending -= 1;
        }
        let class = account.class;
        let served = account.served;
        let now_empty = account.ready.is_empty();
        if account.resident == 0 {
            self.principals.remove(&removed.principal);
        }
        if was_ready && now_empty {
            self.queue_mut(class)
                .ready
                .remove(&(served, removed.principal));
        }
    }

    pub const fn pending(&self) -> usize {
        self.pending
    }
}

#[cfg(test)]
#[path = "governor_tests.rs"]
mod tests;
