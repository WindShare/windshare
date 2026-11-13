use super::metrics::Metrics;
use crate::environment::Trace;
use windshare_relay_core::{
    observation::{Observation, Observer, RelayDecision},
    sessions::{Decision, SessionKey},
};

pub struct NodeTrace {
    metrics: Metrics,
    sessions: Vec<SessionKey>,
}

impl NodeTrace {
    pub const fn new(metrics: Metrics) -> Self {
        Self {
            metrics,
            sessions: Vec::new(),
        }
    }
}

impl Observer for NodeTrace {
    fn observe(&mut self, observation: Observation) {
        match observation {
            Observation::Session(value) => match value.decision {
                Decision::Active => {
                    self.metrics.begin(value.session);
                    if !self.sessions.contains(&value.session) {
                        self.sessions.push(value.session);
                    }
                }
                Decision::Retired(_) => {
                    self.metrics.end(value.session);
                    self.sessions.retain(|session| *session != value.session);
                }
                _ => {}
            },
            Observation::Relay {
                decision:
                    RelayDecision::HandshakeRateLimited
                    | RelayDecision::BusinessCapacity { .. }
                    | RelayDecision::ControlCapacity { .. }
                    | RelayDecision::RegistrationRefused(_),
                ..
            } => self.metrics.admission_refused(),
            Observation::Delivery(value)
                if matches!(
                    value.decision,
                    windshare_relay_core::flow::Decision::Refused(_)
                ) =>
            {
                self.metrics.queue_refused();
            }
            _ => {}
        }
        Trace.observe(observation);
    }
}

impl Drop for NodeTrace {
    fn drop(&mut self) {
        for key in self.sessions.drain(..) {
            self.metrics.end(key);
        }
    }
}
