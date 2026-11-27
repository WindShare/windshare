//! Node-wide pacing, with one fair scheduling identity for both session directions.
mod handoff;
mod pacing;
mod preemption;
mod scheduling;

use crate::connection::{WritePermit, WriteWindow};
use handoff::WriterId;
use scheduling::State;

use super::access::Service;
use pacing::Bandwidth;
use std::{
    sync::{
        Arc, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};
use tokio::{sync::Notify, time::Instant};
use windshare_relay_access::{Attribution, Principal, entitlement::Class};
use windshare_relay_core::{flow::FlowKey, sessions::SessionKey};

const TICK: Duration = Duration::from_millis(5);
const MAX_SESSIONS: usize = crate::config::MAX_RESIDENT_SHARES * crate::config::SHARE_MAX_SESSIONS;
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

#[derive(Clone)]
pub struct Governor {
    state: Arc<Mutex<State>>,
    access: Service,
    wake: Arc<Notify>,
    paced: Arc<AtomicBool>,
    egress: Arc<preemption::Egress>,
}

/// Its lifetime is the flow's queue lifetime, not a single packet's wait.
pub struct LaneLease {
    id: u64,
    governor: Governor,
}

impl Governor {
    pub fn new(access: Service) -> Self {
        Self {
            state: Arc::new(Mutex::new(State::new())),
            access,
            wake: Arc::default(),
            paced: Arc::default(),
            egress: Arc::default(),
        }
    }

    pub(super) fn publish_capacity<T>(
        &self,
        bytes_per_second: Option<u64>,
        public_bytes_per_second: Option<u64>,
        host_uplink: Option<u64>,
        publish_node: impl FnOnce() -> T,
    ) -> T {
        let mut state = self.state.lock().expect("governor");
        let now = Instant::now();
        let node_changed = state.pacing.node.configure(bytes_per_second, now);
        let cap = match (public_bytes_per_second, bytes_per_second) {
            (Some(a), Some(b)) => Some(a.min(b)),
            (a, b) => a.or(b),
        };
        let public_changed = state.pacing.preemption.configure(cap, host_uplink);
        if public_changed {
            state.pacing.public.configure(cap, now);
        }
        // A scheduler pass must see the new rate and its admission policy together.
        let result = publish_node();
        let paced = bytes_per_second.is_some() || public_bytes_per_second.is_some();
        if node_changed || public_changed {
            if paced {
                state.grant_pending(&self.access, None, now);
            } else {
                state.reset();
            }
            self.paced.store(paced, Ordering::Release);
            if state.waiting() {
                self.wake.notify_one();
            }
        }
        let preemption_mode = state.pacing.preemption.mode_name();
        drop(state);
        if node_changed || public_changed {
            tracing::info!(
                event = "governor_capacity_changed",
                ?bytes_per_second,
                ?public_bytes_per_second,
                ?host_uplink,
                preemption_mode,
            );
        }
        result
    }

    #[cfg(test)]
    pub(crate) fn exhaust_initial_public_credit(&self) {
        let mut state = self.state.lock().unwrap();
        let initial = state.pacing.replenish(Instant::now());
        let mut remaining = initial;
        assert!(remaining.reserve(
            windshare_relay_access::entitlement::Class::Lowest,
            MAX_FRAME
        ));
        state.pacing.charge(&initial, &remaining);
    }

    #[cfg(test)]
    pub(super) fn capacity(&self) -> Option<u64> {
        self.state.lock().unwrap().pacing.node.capacity()
    }

    pub(crate) fn record_egress(&self, owner: impl FnOnce() -> bool, bytes: u64) {
        if self.paced.load(Ordering::Relaxed) {
            self.egress.record(owner(), bytes);
        }
    }

    pub(crate) fn register(
        &self,
        flow: FlowKey,
        attribution: Attribution,
        wake: Arc<Notify>,
        window: Arc<WriteWindow>,
    ) -> Option<LaneLease> {
        let mut state = self.state.lock().expect("governor");
        let id = state.register(Key::from(flow.session()), attribution, wake, window)?;
        drop(state);
        Some(LaneLease {
            id,
            governor: self.clone(),
        })
    }

    /// Advertise each flow's writable lookahead together, so the first flow
    /// visited by the actor cannot take bandwidth before its peers compete.
    pub(crate) fn take<'a, H, I>(
        &self,
        window: &WriteWindow,
        mut flows: I,
    ) -> Option<(usize, WritePermit)>
    where
        H: Iterator<Item = u64>,
        I: ExactSizeIterator<Item = (&'a LaneLease, H)> + Clone,
    {
        // With one local flow and no outstanding scheduled work, the socket is
        // the only bottleneck. The ordinary unpaced path takes no node-wide lock.
        if flows.len() == 1
            && !self.paced.load(Ordering::Acquire)
            && !window.scheduled.load(Ordering::Relaxed)
        {
            let bytes = flows.next()?.1.next()?;
            return window.reserve(bytes).map(|permit| (0, permit));
        }
        let mut state = self.state.lock().expect("governor");
        let now = Instant::now();
        let preemption = state.pacing.observe_wire(&self.egress, now);
        let writer = WriterId::from(window);
        let reclassified = state.refresh_classes(&self.access, super::now().unsigned_abs());
        let was_waiting = state.waiting();
        for (lane, sizes) in flows.clone() {
            state.offer(lane.id, sizes, &self.access, now);
        }
        state.grant_pending(&self.access, Some(writer), now);
        let grant = state.take(writer).map(|(id, permit)| {
            (
                flows
                    .into_iter()
                    .position(|(lane, _)| lane.id == id)
                    .expect("destination owns its lanes"),
                permit,
            )
        });
        if !was_waiting && state.waiting() {
            self.wake.notify_one();
        }
        drop(state);
        log_preemption(preemption);
        log_reclassifications(reclassified);
        grant
    }

    pub async fn run(&self) {
        let mut tick = tokio::time::interval(TICK);
        tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            if self.pump() {
                tick.tick().await;
            } else {
                self.wake.notified().await;
                tick.reset();
            }
        }
    }

    fn pump(&self) -> bool {
        let mut state = self.state.lock().expect("governor");
        let reclassified = state.refresh_classes(&self.access, super::now().unsigned_abs());
        let now = Instant::now();
        let preemption = state.pacing.observe_wire(&self.egress, now);
        state.grant_pending(&self.access, None, now);
        let waiting = state.waiting();
        drop(state);
        log_preemption(preemption);
        log_reclassifications(reclassified);
        waiting
    }
}

fn log_preemption(transition: Option<preemption::Transition>) {
    if let Some(transition) = transition {
        tracing::debug!(
            event = "owner_preemption",
            mode = transition.mode,
            previous_phase = transition.previous_phase,
            phase = transition.phase,
            owner_bytes_per_second = transition.owner_bytes_per_second,
            public_bytes_per_second = transition.public_bytes_per_second,
            previous = transition.previous_limit,
            public_limit = transition.public_limit,
        );
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

impl Drop for LaneLease {
    fn drop(&mut self) {
        self.governor
            .state
            .lock()
            .expect("governor")
            .remove(self.id, &self.governor.access);
    }
}

#[cfg(test)]
#[path = "governor/governor_tests.rs"]
mod tests;
