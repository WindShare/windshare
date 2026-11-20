//! Node-wide pacing, with one fair scheduling identity for both session directions.
mod pacing;

use super::access::Service;
use pacing::Bandwidth;
use std::{
    collections::{BTreeMap, VecDeque},
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};
use tokio::{sync::Notify, time::Instant};
use windshare_relay_access::{
    Attribution, Principal,
    entitlement::Class,
    governor::{Demand, Scheduler},
};
use windshare_relay_core::{flow::FlowKey, sessions::SessionKey};

const TICK: Duration = Duration::from_millis(5);
const MAX_SESSIONS: usize = crate::config::MAX_RESIDENT_SHARES * 64;
const MAX_FRAME: u64 = windshare_relay_core::sessions::OpaqueHeader::MAX_ENCODED_BYTES as u64;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Ord, PartialOrd)]
struct Key([u8; 12], [u8; 16], u64);

impl From<SessionKey> for Key {
    fn from(session: SessionKey) -> Self {
        let admission = session.admission();
        Self(
            *session.share().as_bytes(),
            *admission.incarnation().as_bytes(),
            admission.sequence().get(),
        )
    }
}

struct Lane {
    session: Key,
    attribution: Attribution,
    wake: Arc<Notify>,
    demand: Option<u64>,
    granted: bool,
}

struct State {
    scheduler: Scheduler<Key>,
    queues: BTreeMap<Key, VecDeque<u64>>,
    references: BTreeMap<Key, usize>,
    lanes: BTreeMap<u64, Lane>,
    sequence: u64,
    pacing: Bandwidth,
    classified: u64,
}

#[derive(Clone)]
pub struct Governor {
    state: Arc<Mutex<State>>,
    access: Service,
    wake: Arc<Notify>,
    pacing_enabled: Arc<AtomicBool>,
}

/// Its lifetime is the flow's queue lifetime, not a single packet's wait.
pub struct LaneLease {
    id: u64,
    governor: Governor,
}

impl Governor {
    pub fn new(access: Service) -> Self {
        Self {
            state: Arc::new(Mutex::new(State {
                scheduler: Scheduler::new(MAX_SESSIONS, MAX_FRAME),
                queues: BTreeMap::new(),
                references: BTreeMap::new(),
                lanes: BTreeMap::new(),
                sequence: 0,
                pacing: Bandwidth::default(),
                classified: 0,
            })),
            access,
            wake: Arc::new(Notify::new()),
            pacing_enabled: Arc::new(AtomicBool::new(false)),
        }
    }

    pub(super) fn publish_capacity<T>(
        &self,
        bytes_per_second: Option<u64>,
        public_bytes_per_second: Option<u64>,
        publish_node: impl FnOnce() -> T,
    ) -> T {
        let mut state = self.state.lock().expect("governor");
        let now = Instant::now();
        let node_changed = state.pacing.node.configure(bytes_per_second, now);
        let public_changed = state.pacing.public.configure(public_bytes_per_second, now);
        let enabled = bytes_per_second.is_some() || public_bytes_per_second.is_some();
        // A scheduler pass must see the new rate and its admission policy together.
        let result = publish_node();
        self.pacing_enabled.store(enabled, Ordering::Release);
        let mut wake_governor = false;
        if !enabled {
            // Release any previously throttled lanes so the unpaced fast path
            // never leaves stale queue entries behind.
            let sessions: Vec<_> = state.queues.keys().copied().collect();
            for session in sessions {
                state.scheduler.cancel(session);
            }
            state.queues.clear();
            for lane in state.lanes.values_mut() {
                if lane.demand.take().is_some() {
                    lane.granted = false;
                    lane.wake.notify_one();
                }
            }
        } else if node_changed || public_changed {
            state.grant_pending(&self.access, |_, wake| wake.notify_one());
            wake_governor = state.has_waiting_demand();
        }
        drop(state);
        if wake_governor {
            self.wake.notify_one();
        }
        if node_changed || public_changed {
            tracing::info!(
                event = "governor_capacity_changed",
                ?bytes_per_second,
                ?public_bytes_per_second
            );
        }
        result
    }

    #[cfg(test)]
    pub(super) fn capacity(&self) -> Option<u64> {
        self.state.lock().unwrap().pacing.node.capacity()
    }

    pub fn register(
        &self,
        flow: FlowKey,
        attribution: Attribution,
        wake: Arc<Notify>,
    ) -> Option<LaneLease> {
        let mut state = self.state.lock().expect("governor");
        if state.lanes.len() >= MAX_SESSIONS * 2 {
            return None;
        }
        state.sequence = state.sequence.checked_add(1)?;
        let id = state.sequence;
        let session = Key::from(flow.session());
        state.lanes.insert(
            id,
            Lane {
                session,
                attribution,
                wake,
                demand: None,
                granted: false,
            },
        );
        *state.references.entry(session).or_default() += 1;
        drop(state);
        Some(LaneLease {
            id,
            governor: self.clone(),
        })
    }

    pub async fn run(&self) {
        let mut tick = tokio::time::interval(TICK);
        tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            if self.pump() {
                tick.tick().await;
            } else {
                // Sleep at 0Hz until a paced lane actually blocks on token refill.
                self.wake.notified().await;
                tick.reset();
            }
        }
    }

    fn pump(&self) -> bool {
        let mut state = self.state.lock().expect("governor");
        let reclassified = state.refresh_classes(&self.access);
        state.grant_pending(&self.access, |_, wake| wake.notify_one());
        let active = state.has_waiting_demand();
        drop(state);
        log_reclassifications(reclassified);
        active
    }
}

impl State {
    fn has_waiting_demand(&self) -> bool {
        self.queues.values().any(|queue| !queue.is_empty())
    }
    fn demand(&self, id: u64, access: &Service) -> Demand {
        let lane = self.lanes.get(&id).expect("queued lane");
        Demand {
            principal: lane.attribution.principal(lane.session.0),
            class: access
                .class(lane.attribution, super::now().unsigned_abs())
                .0,
            bytes: lane.demand.expect("queued demand"),
        }
    }

    fn refresh_classes(&mut self, access: &Service) -> Vec<(Principal, Class, Class)> {
        let second = super::now().unsigned_abs();
        if self.classified == second {
            return Vec::new();
        }
        access.expire_quotas(second);
        let mut reclassified = Vec::new();
        for lane in self.lanes.values() {
            let principal = lane.attribution.principal(lane.session.0);
            let to = access.class(lane.attribution, second).0;
            if let Some(from) = self.scheduler.reclassify(principal, to) {
                reclassified.push((principal, from, to));
            }
        }
        self.classified = second;
        reclassified
    }

    fn grant_pending(&mut self, access: &Service, mut on_grant: impl FnMut(u64, &Notify)) {
        let available = self.pacing.replenish(Instant::now());
        let mut remaining = available;
        while let Some((session, _)) = self
            .scheduler
            .next(|class, bytes| remaining.reserve(class, bytes))
        {
            let queue = self.queues.get_mut(&session).expect("scheduled session");
            let id = queue.pop_front().expect("scheduled lane");
            let next = queue.front().copied();
            let lane = self.lanes.get_mut(&id).expect("scheduled lane");
            lane.granted = true;
            on_grant(id, &lane.wake);
            if let Some(next) = next {
                let demand = self.demand(next, access);
                assert!(self.scheduler.request(session, demand));
            }
        }
        self.pacing.charge(&available, &remaining);
    }
}

fn log_reclassifications(reclassified: Vec<(Principal, Class, Class)>) {
    for (principal, from, to) in reclassified {
        tracing::info!(
            event = "governor_principal_reclassified",
            ?principal,
            ?from,
            ?to,
        );
    }
}

impl LaneLease {
    /// Atomically checks pacing credit and consumes the grant in one lock
    /// acquisition, bypassing the global governor lock entirely when unpaced.
    pub fn try_take(&self, bytes: u64) -> bool {
        if !self.governor.pacing_enabled.load(Ordering::Acquire) {
            return true;
        }
        let mut state = self.governor.state.lock().expect("governor");
        if !self.governor.pacing_enabled.load(Ordering::Relaxed) {
            return true;
        }
        let lane = state.lanes.get_mut(&self.id).expect("live lane");
        if let Some(demand) = lane.demand {
            if demand == bytes && lane.granted {
                lane.demand = None;
                lane.granted = false;
                return true;
            }
            return false;
        }
        lane.demand = Some(bytes);
        let session = lane.session;
        let queue = state.queues.entry(session).or_default();
        let schedule = queue.is_empty();
        queue.push_back(self.id);
        let reclassified = state.refresh_classes(&self.governor.access);
        if schedule {
            let demand = state.demand(self.id, &self.governor.access);
            assert!(state.scheduler.request(session, demand));
        }
        // The active caller can take its grant directly. Other lanes may be waiting.
        let mut granted = false;
        state.grant_pending(&self.governor.access, |id, wake| {
            if id == self.id {
                granted = true;
            } else {
                wake.notify_one();
            }
        });
        if granted {
            let lane = state.lanes.get_mut(&self.id).expect("live lane");
            lane.demand = None;
            lane.granted = false;
        }
        let wake_governor = !granted || state.has_waiting_demand();
        drop(state);
        if wake_governor {
            self.governor.wake.notify_one();
        }
        log_reclassifications(reclassified);
        granted
    }
}

impl Drop for LaneLease {
    fn drop(&mut self) {
        let mut state = self.governor.state.lock().expect("governor");
        let Some(lane) = state.lanes.remove(&self.id) else {
            return;
        };
        if let Some(queue) = state.queues.get_mut(&lane.session) {
            let was_head = queue.front() == Some(&self.id);
            queue.retain(|id| *id != self.id);
            let next = queue.front().copied();
            if was_head {
                // Replacing a cancelled head must retain the session's service debt.
                state.scheduler.withdraw(lane.session);
                if let Some(next) = next {
                    let demand = state.demand(next, &self.governor.access);
                    assert!(state.scheduler.request(lane.session, demand));
                }
            }
        }
        let references = state
            .references
            .get_mut(&lane.session)
            .expect("lane session");
        *references -= 1;
        if *references == 0 {
            state.references.remove(&lane.session);
            state.queues.remove(&lane.session);
            state.scheduler.cancel(lane.session);
        }
    }
}

#[cfg(test)]
#[path = "governor/governor_tests.rs"]
mod tests;
