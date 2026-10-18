//! Native scheduling indexes live flows, never mirrors their accounting.
use crate::config::DELIVERY_WINDOW;
use std::collections::{BTreeMap, BTreeSet, VecDeque};
use windshare_relay_core::{
    flow::{Counts, FlowKey},
    lifecycle::ConnectionRef,
    observation::Observer,
    share::ShareRelay,
};

#[derive(Default)]
pub(super) struct Windows {
    destinations: BTreeMap<ConnectionRef, VecDeque<FlowKey>>,
    ready: BTreeSet<ConnectionRef>,
    blocked: BTreeMap<ConnectionRef, BTreeSet<ConnectionRef>>,
}

impl Windows {
    pub fn track(&mut self, flow: FlowKey) {
        let flows = self.destinations.entry(flow.destination()).or_default();
        if !flows.contains(&flow) {
            flows.push_back(flow);
            self.ready.insert(flow.destination());
        }
    }

    pub fn wake(&mut self, destination: ConnectionRef) {
        if self.destinations.contains_key(&destination) {
            self.ready.insert(destination);
        }
    }

    pub fn remove(&mut self, flow: FlowKey) {
        let destination = flow.destination();
        let source = flow.permit().source();
        if let Some(destinations) = self.blocked.get_mut(&source) {
            destinations.remove(&destination);
            if destinations.is_empty() {
                self.blocked.remove(&source);
            }
        }
        if let Some(flows) = self.destinations.get_mut(&destination) {
            flows.retain(|value| *value != flow);
            if flows.is_empty() {
                self.destinations.remove(&destination);
                self.ready.remove(&destination);
            } else {
                self.ready.insert(destination);
            }
        }
    }

    pub fn writable(&mut self, source: ConnectionRef) {
        if let Some(destinations) = self.blocked.remove(&source) {
            self.ready.extend(destinations);
        }
    }

    pub fn next<O: Observer>(
        &mut self,
        relay: &ShareRelay<O>,
        available: impl Fn(ConnectionRef) -> bool,
    ) -> Option<(FlowKey, Counts)> {
        while let Some(destination) = self.ready.pop_first() {
            let flows = self.destinations.get_mut(&destination)?;
            for _ in 0..flows.len() {
                let flow = flows.pop_front().expect("bounded flow rotation");
                flows.push_back(flow);
                let Some(credit) = relay.delivery_window(flow, DELIVERY_WINDOW) else {
                    continue;
                };
                if credit != Counts::ZERO {
                    if !available(flow.permit().source()) {
                        // Defer reservations until their control can be queued.
                        // Writer readiness resumes this without polling or refunds.
                        self.blocked
                            .entry(flow.permit().source())
                            .or_default()
                            .insert(destination);
                        continue;
                    }
                    // Rotate after each offer so shared destination capacity freed
                    // by one session can also wake a waiting sibling.
                    self.ready.insert(destination);
                    return Some((flow, credit));
                }
            }
        }
        None
    }
}
