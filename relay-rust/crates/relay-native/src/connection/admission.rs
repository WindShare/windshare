//! Global and source capacity stay with every task that can retain socket resources.
use super::ConnectionLimits;
use crate::node::access::Bearer;
use anyhow::{Result, anyhow};
use std::{
    collections::BTreeMap,
    fmt,
    net::IpAddr,
    sync::{Arc, Mutex, OnceLock},
};
use windshare_relay_core::{
    clock::UnixMillis,
    handshake::Initiation,
    lifecycle::ConnectionRef,
    share::{AttemptLimiter, AttemptRate},
};

const OWNER_CAPACITY_DIVISOR: usize = 8;
const AUTHENTICATED_CAPACITY_DIVISOR: usize = 8;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AdmissionPriority {
    Anonymous,
    Authenticated,
    Owner,
}

const fn admission_limit(total: usize, priority: AdmissionPriority) -> usize {
    match priority {
        AdmissionPriority::Owner => total,
        AdmissionPriority::Authenticated => total - total / OWNER_CAPACITY_DIVISOR,
        AdmissionPriority::Anonymous => {
            total - total / OWNER_CAPACITY_DIVISOR - total / AUTHENTICATED_CAPACITY_DIVISOR
        }
    }
}

pub struct Capacity {
    limits: ConnectionLimits,
    state: Mutex<State>,
}

struct State {
    total: Usage,
    sources: BTreeMap<IpAddr, Usage>,
    attempts: AttemptLimiter,
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
struct Usage {
    business: usize,
    controls: usize,
}

#[derive(Clone, Copy, Debug)]
enum Slot {
    Control,
    Business,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Refusal {
    GlobalControl,
    SourceControl,
    GlobalBusiness,
    SourceBusiness,
    AttemptRate,
}

impl fmt::Display for Refusal {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::GlobalControl => "global_control_capacity",
            Self::SourceControl => "source_control_capacity",
            Self::GlobalBusiness => "global_business_capacity",
            Self::SourceBusiness => "source_business_capacity",
            Self::AttemptRate => "connection_attempt_rate",
        })
    }
}

impl std::error::Error for Refusal {}

pub struct Admission {
    pub(crate) usage: crate::node::usage::Meter,
    priority: AdmissionPriority,
    capacity: Arc<Capacity>,
    source: IpAddr,
    connection: ConnectionRef,
    slot: Mutex<Slot>,
    bearer: OnceLock<Bearer>,
}

impl Capacity {
    pub fn new(
        limits: ConnectionLimits,
        attempts: AttemptRate,
        now: UnixMillis,
    ) -> Result<Arc<Self>> {
        limits.validate()?;
        Ok(Arc::new(Self {
            limits,
            state: Mutex::new(State {
                total: Usage::default(),
                sources: BTreeMap::new(),
                attempts: AttemptLimiter::new(attempts, now)
                    .map_err(|error| anyhow!("invalid ingress rate: {error:?}"))?,
            }),
        }))
    }

    pub fn accept(
        self: &Arc<Self>,
        source: IpAddr,
        connection: ConnectionRef,
        now: UnixMillis,
        priority: AdmissionPriority,
    ) -> Result<Arc<Admission>, Refusal> {
        let source = source.to_canonical();
        let mut state = self.state.lock().expect("admission lock");
        state.accept(self.limits, source, now, priority).inspect_err(|reason| {
            tracing::debug!(event = "connection_admission_refused", ?connection, %source, %reason);
        })?;
        drop(state);
        tracing::debug!(event = "control_connection_admitted", ?connection, %source);
        Ok(Arc::new(Admission {
            usage: crate::node::usage::Meter::default(),
            priority,
            capacity: Arc::clone(self),
            source,
            connection,
            slot: Mutex::new(Slot::Control),
            bearer: OnceLock::new(),
        }))
    }
}

impl State {
    fn accept(
        &mut self,
        limits: ConnectionLimits,
        source: IpAddr,
        now: UnixMillis,
        priority: AdmissionPriority,
    ) -> Result<(), Refusal> {
        if self.total.controls >= admission_limit(limits.controls, priority) {
            return Err(Refusal::GlobalControl);
        }
        if self
            .sources
            .get(&source)
            .copied()
            .unwrap_or_default()
            .controls
            >= admission_limit(limits.controls_per_source, priority)
        {
            return Err(Refusal::SourceControl);
        }
        if !self.attempts.attempt(now) {
            return Err(Refusal::AttemptRate);
        }
        self.total.controls += 1;
        self.sources.entry(source).or_default().controls += 1;
        Ok(())
    }

    fn promote(
        &mut self,
        limits: ConnectionLimits,
        source: IpAddr,
        priority: AdmissionPriority,
    ) -> Result<(), Refusal> {
        if self.total.business >= admission_limit(limits.business, priority) {
            return Err(Refusal::GlobalBusiness);
        }
        let usage = self.sources.get_mut(&source).expect("admitted source");
        if usage.business >= admission_limit(limits.business_per_source, priority) {
            return Err(Refusal::SourceBusiness);
        }
        self.total.business += 1;
        usage.business += 1;
        self.total.controls -= 1;
        usage.controls -= 1;
        Ok(())
    }

    fn release(&mut self, source: IpAddr, slot: Slot) {
        let usage = self.sources.get_mut(&source).expect("admitted source");
        match slot {
            Slot::Control => {
                self.total.controls -= 1;
                usage.controls -= 1;
            }
            Slot::Business => {
                self.total.business -= 1;
                usage.business -= 1;
            }
        }
        if *usage == Usage::default() {
            self.sources.remove(&source);
        }
    }
}

impl Admission {
    pub const fn priority(&self) -> AdmissionPriority {
        self.priority
    }

    pub(crate) fn set_bearer(&self, bearer: Bearer) {
        let _ = self.bearer.set(bearer);
    }
    pub(crate) fn bearer(&self) -> Option<&Bearer> {
        self.bearer.get()
    }
    pub const fn source(&self) -> IpAddr {
        self.source
    }

    pub fn inspect(&self, frame: &[u8]) -> Result<(), Refusal> {
        // Parsing here reserves resources only. The kernel remains the sole
        // authority for authentication, phase ordering and per-share admission.
        if !Initiation::decode(frame).is_ok_and(|intent| intent.uses_business_capacity()) {
            return Ok(());
        }
        let mut slot = self.slot.lock().expect("admission lock");
        if matches!(*slot, Slot::Business) {
            return Ok(());
        }
        self.capacity
            .state
            .lock()
            .expect("admission lock")
            .promote(self.capacity.limits, self.source, self.priority)
            .inspect_err(|reason| {
                tracing::debug!(event = "connection_admission_refused",
                    connection = ?self.connection, source = %self.source, %reason);
            })?;
        *slot = Slot::Business;
        drop(slot);
        tracing::debug!(event = "business_connection_admitted",
            connection = ?self.connection, source = %self.source);
        Ok(())
    }
}

impl Drop for Admission {
    fn drop(&mut self) {
        let slot = *self.slot.get_mut().expect("admission lock");
        self.capacity
            .state
            .lock()
            .expect("admission lock")
            .release(self.source, slot);
        tracing::debug!(event = "connection_admission_released",
            connection = ?self.connection, source = %self.source, ?slot);
    }
}

#[cfg(test)]
#[path = "admission_priority_tests.rs"]
mod priority_tests;
#[cfg(test)]
#[path = "admission_tests.rs"]
mod tests;
