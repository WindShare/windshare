//! Node-wide pacing, with one fair scheduling identity for both session directions.
mod pacing;
mod scheduling;

use crate::connection::{WritePermit, WriteWindow};
use scheduling::State;

use super::access::Service;
use pacing::Bandwidth;
use std::{
    sync::{Arc, Mutex},
    time::Duration,
};
use tokio::{sync::Notify, time::Instant};
use windshare_relay_access::{Attribution, Principal, entitlement::Class};
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
            state: Arc::new(Mutex::new(State::new())),
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
        flows: I,
    ) -> Option<(usize, WritePermit)>
    where
        H: Iterator<Item = u64>,
        I: Iterator<Item = (&'a LaneLease, H)> + Clone,
    {
        let mut state = self.state.lock().expect("governor");
        let reclassified = state.refresh_classes(&self.access, super::now().unsigned_abs());
        for (lane, sizes) in flows.clone() {
            state.offer(lane.id, sizes, &self.access);
        }
        state.grant_pending(&self.access, Some(window));
        let grant = state.take_first(flows.map(|(lane, _)| lane.id));
        drop(state);
        log_reclassifications(reclassified);
        grant
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
        let reclassified = state.refresh_classes(&self.access, super::now().unsigned_abs());
        state.grant_pending(&self.access, None);
        drop(state);
        log_reclassifications(reclassified);
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
