//! Node-wide pacing, with one fair scheduling identity for both session directions.
mod pacing;

use super::access::Service;
use pacing::Bandwidth;
use std::{
    collections::{BTreeMap, VecDeque},
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::{sync::Notify, time::Instant};
use windshare_relay_access::{
    Attribution,
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
        // A scheduler pass must see the new rate and its admission policy together.
        let result = publish_node();
        drop(state);
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
            tick.tick().await;
            self.pump();
        }
    }

    fn pump(&self) {
        let mut state = self.state.lock().expect("governor");
        state.grant_pending(&self.access, |_, wake| wake.notify_one());
    }
}

impl State {
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

    fn grant_pending(&mut self, access: &Service, mut on_grant: impl FnMut(u64, &Notify)) {
        let available = self.pacing.replenish(Instant::now());
        let second = super::now().unsigned_abs();
        if self.classified != second {
            access.expire_quotas(second);
            for lane in self.lanes.values() {
                self.scheduler.reclassify(
                    lane.attribution.principal(lane.session.0),
                    access.class(lane.attribution, second).0,
                );
            }
            self.classified = second;
        }
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

impl LaneLease {
    pub fn ready(&self, bytes: u64) -> bool {
        let mut state = self.governor.state.lock().expect("governor");
        let lane = state.lanes.get_mut(&self.id).expect("live lane");
        if let Some(demand) = lane.demand {
            return demand == bytes && lane.granted;
        }
        lane.demand = Some(bytes);
        let session = lane.session;
        let queue = state.queues.entry(session).or_default();
        let schedule = queue.is_empty();
        queue.push_back(self.id);
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
        granted
    }

    pub fn submitted(&self) {
        let mut state = self.governor.state.lock().expect("governor");
        let lane = state.lanes.get_mut(&self.id).expect("live lane");
        lane.demand = None;
        lane.granted = false;
        drop(state);
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
