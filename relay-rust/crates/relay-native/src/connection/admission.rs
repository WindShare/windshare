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

const OWNER_CAPACITY_DIVISOR: u32 = 8;
const AUTHENTICATED_CAPACITY_DIVISOR: u32 = 8;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AdmissionPriority {
    Anonymous,
    Authenticated,
    Owner,
}

const fn admission_limit(total: usize, priority: AdmissionPriority) -> usize {
    let owner = total / OWNER_CAPACITY_DIVISOR as usize;
    let authenticated = total / AUTHENTICATED_CAPACITY_DIVISOR as usize;
    match priority {
        AdmissionPriority::Owner => total,
        AdmissionPriority::Authenticated => total - owner,
        AdmissionPriority::Anonymous => total - owner - authenticated,
    }
}

const fn attempt_reserve(burst: u32, priority: AdmissionPriority) -> u32 {
    let owner = burst / OWNER_CAPACITY_DIVISOR;
    let authenticated = burst / AUTHENTICATED_CAPACITY_DIVISOR;
    match priority {
        AdmissionPriority::Owner => 0,
        AdmissionPriority::Authenticated => owner,
        AdmissionPriority::Anonymous => owner + authenticated,
    }
}

pub struct Capacity {
    limits: ConnectionLimits,
    pub(crate) access: OnceLock<crate::node::access::Service>,
    state: Mutex<State>,
}

struct State {
    public_business: usize,
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
    Business(AdmissionPriority),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Refusal {
    GlobalControl,
    SourceControl,
    GlobalBusiness,
    PublicBusiness,
    SourceBusiness,
    AttemptRate,
}

impl fmt::Display for Refusal {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::GlobalControl => "global_control_capacity",
            Self::SourceControl => "source_control_capacity",
            Self::PublicBusiness => "public_connection_limit",
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
    share_attribution: Arc<OnceLock<windshare_relay_access::Attribution>>,
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
            access: OnceLock::new(),
            state: Mutex::new(State {
                public_business: 0,
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
        state
            .accept(self.limits, source, now, priority)
            .inspect_err(|reason| {
                tracing::debug!(
                    event = "connection_admission_refused",
                    ?connection,
                    %source,
                    ?priority,
                    %reason
                );
            })?;
        drop(state);
        tracing::debug!(
            event = "control_connection_admitted",
            ?connection,
            %source,
            ?priority
        );
        Ok(Arc::new(Admission {
            usage: crate::node::usage::Meter::default(),
            priority,
            capacity: Arc::clone(self),
            source,
            connection,
            slot: Mutex::new(Slot::Control),
            bearer: OnceLock::new(),
            share_attribution: Arc::new(OnceLock::new()),
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
        let reserve = attempt_reserve(self.attempts.burst(), priority);
        if !self.attempts.attempt_with_reserve(now, reserve) {
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
        public_limit: usize,
    ) -> Result<(), Refusal> {
        if priority != AdmissionPriority::Owner && self.public_business >= public_limit {
            return Err(Refusal::PublicBusiness);
        }
        if self.total.business >= admission_limit(limits.business, priority) {
            return Err(Refusal::GlobalBusiness);
        }
        let usage = self.sources.get_mut(&source).expect("admitted source");
        if usage.business >= admission_limit(limits.business_per_source, priority) {
            return Err(Refusal::SourceBusiness);
        }
        self.public_business += usize::from(priority != AdmissionPriority::Owner);
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
            Slot::Business(priority) => {
                self.public_business -= usize::from(priority != AdmissionPriority::Owner);
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
    pub(crate) fn share_attribution(&self) -> Arc<OnceLock<windshare_relay_access::Attribution>> {
        Arc::clone(&self.share_attribution)
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
        if matches!(*slot, Slot::Business(_)) {
            return Ok(());
        }
        self.capacity
            .state
            .lock()
            .expect("admission lock")
            .promote(
                self.capacity.limits,
                self.source,
                self.priority,
                self.capacity
                    .access
                    .get()
                    .map_or(usize::MAX, crate::node::access::Service::public_connections),
            )
            .inspect_err(|reason| {
                tracing::debug!(
                    event = "connection_admission_refused",
                    connection = ?self.connection,
                    source = %self.source,
                    priority = ?self.priority,
                    %reason
                );
            })?;
        *slot = Slot::Business(self.priority);
        drop(slot);
        tracing::debug!(
            event = "business_connection_admitted",
            connection = ?self.connection,
            source = %self.source,
            priority = ?self.priority
        );
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
        tracing::debug!(
            event = "connection_admission_released",
            connection = ?self.connection,
            source = %self.source,
            priority = ?self.priority,
            ?slot
        );
    }
}

#[cfg(test)]
#[path = "admission_priority_tests.rs"]
mod priority_tests;
#[cfg(test)]
#[path = "admission_tests.rs"]
mod tests;
