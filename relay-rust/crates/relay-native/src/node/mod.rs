//! Node authority is shared by admission, discovery and every existing transport.
pub mod access;
mod budget;
pub(crate) mod governor;
mod identity;
mod metered;
mod metrics;
pub(crate) mod usage;
pub use identity::Identity;
mod configuration;
pub(crate) mod contribution;
mod descriptor;
mod reload;
mod settings;
mod trace;
pub use configuration::Configuration;

pub(crate) use budget::WriteOrigin;
pub(crate) use descriptor::{GossipDescriptor, GossipEnvelope};
pub(crate) use metered::Metered;
pub(crate) use metrics::Metrics;
pub use metrics::Snapshot;
pub use settings::{Ceiling, ContributionLimits, MonthlyBudget, Services, Settings};
pub(crate) use trace::NodeTrace;

use crate::storage::Database;
use anyhow::{Result, anyhow};
use std::{
    sync::{
        Arc, Mutex, MutexGuard, OnceLock,
        atomic::{AtomicBool, AtomicI64, AtomicU8, AtomicU64, Ordering},
    },
    time::{SystemTime, UNIX_EPOCH},
};
use tokio::sync::watch;

pub(crate) const SERVICE_RETRY_SECONDS: u32 = 30;
pub(crate) const SERVICE_RETRY_AFTER: windshare_relay_core::error_frame::RetryAfter =
    windshare_relay_core::error_frame::RetryAfter::from_millis(SERVICE_RETRY_SECONDS * 1000)
        .expect("service retry must fit a WS2E frame");

struct State {
    bound_stun_ports: Option<Vec<u16>>,
    settings: Settings,
    control: Option<Database>,
    budgets: budget::Budgets,
    unavailable: bool,
    storage_failed: bool,
    unpaced_relay_warned: bool,
}

#[derive(Clone)]
pub struct Runtime {
    pub(crate) access: access::Service,
    pub(crate) console: Arc<OnceLock<crate::console::Console>>,
    pub(crate) governor: governor::Governor,
    identity: Arc<OnceLock<descriptor::InstalledIdentity>>,
    state: Arc<Mutex<State>>,
    pub(crate) local_contribution: Arc<OnceLock<contribution::Control>>,
    control_writer: Arc<Mutex<()>>,
    reporting_authority: Arc<OnceLock<Option<usage::Authority>>>,
    bootstrap_nodes: watch::Sender<Arc<[String]>>,
    gossip_cache: Arc<Mutex<Option<descriptor::CachedGossip>>>,
    descriptor_revision: Arc<AtomicU64>,
    revocations: watch::Sender<u64>,
    epoch: Arc<AtomicU64>,
    services: Arc<AtomicU8>,
    unavailable: Arc<AtomicBool>,
    has_budgets: Arc<AtomicBool>,
    next_rollover: Arc<AtomicI64>,
    last_config_bytes: Arc<Mutex<Option<Vec<u8>>>>,
    pub(crate) metrics: Metrics,
    pub(crate) usage: usage::Collector,
}

impl Default for Runtime {
    fn default() -> Self {
        let access = access::Service::default();
        let default_services = Services::default();
        Self {
            governor: governor::Governor::new(access.clone()),
            access,
            console: Arc::default(),
            identity: Arc::default(),
            state: Arc::new(Mutex::new(State {
                bound_stun_ports: None,
                settings: Settings::default(),
                control: None,
                budgets: budget::Budgets::default(),
                unavailable: false,
                storage_failed: false,
                unpaced_relay_warned: false,
            })),
            local_contribution: Arc::default(),
            control_writer: Arc::default(),
            reporting_authority: Arc::default(),
            bootstrap_nodes: watch::channel(Arc::from([])).0,
            gossip_cache: Arc::default(),
            descriptor_revision: Arc::new(AtomicU64::new(0)),
            revocations: watch::channel(0).0,
            epoch: Arc::new(AtomicU64::new(0)),
            services: Arc::new(AtomicU8::new(default_services.to_bits())),
            unavailable: Arc::new(AtomicBool::new(false)),
            has_budgets: Arc::new(AtomicBool::new(false)),
            next_rollover: Arc::new(AtomicI64::new(i64::MAX)),
            last_config_bytes: Arc::default(),
            metrics: Metrics::default(),
            usage: usage::Collector::default(),
        }
    }
}

impl Runtime {
    #[cfg(test)]
    pub(crate) fn new(settings: Settings, store: Database) -> Result<Self> {
        let runtime = Self::default();
        runtime.install_settings(settings, store)?;
        Ok(runtime)
    }

    pub(crate) fn configure(&self, settings: Settings, store: Database) -> Result<()> {
        if settings.authority.is_none() {
            self.local_contribution
                .get_or_init(|| contribution::Control::new(store.clone()));
        }
        self.install_settings(settings, store)
    }

    fn install_settings(&self, settings: Settings, store: Database) -> Result<()> {
        let generations = settings.validate()?;
        let writer = self.lock_control()?;
        let timestamp = now();
        let access_now = timestamp.unsigned_abs();
        self.access
            .restore_access(store.initialize_control(access_now)?, access_now)?;
        let budgets = budget::Budgets::restore(store.load_budgets()?);
        let mut state = self.state.lock().map_err(|_| anyhow!("node state"))?;
        state.control = Some(store);
        state.budgets = budgets;
        drop(state);
        let result = self.apply_settings(settings, &generations, timestamp);
        drop(writer);
        result
    }

    // Durable reservations and authority updates must not replace records
    // between preparing a configuration and publishing its committed state.
    fn lock_control(&self) -> Result<MutexGuard<'_, ()>> {
        self.control_writer
            .lock()
            .map_err(|_| anyhow!("node control writer poisoned"))
    }

    pub fn services(&self) -> Services {
        Services::from_bits(self.services.load(Ordering::Acquire))
    }

    pub fn available(&self) -> bool {
        !self.unavailable.load(Ordering::Acquire)
    }

    pub(crate) fn tracks_egress_attribution(&self) -> bool {
        self.usage.is_configured() || self.has_budgets.load(Ordering::Acquire)
    }

    pub fn snapshot(&self) -> Snapshot {
        self.metrics.snapshot()
    }

    pub(crate) fn epoch(&self) -> u64 {
        self.epoch.load(Ordering::Acquire)
    }

    pub(crate) fn subscribe_bootstrap_nodes(&self) -> watch::Receiver<Arc<[String]>> {
        self.bootstrap_nodes.subscribe()
    }

    pub(super) fn bump_descriptor_revision(&self) {
        self.descriptor_revision.fetch_add(1, Ordering::Release);
    }

    pub(crate) async fn revoked(&self, epoch: u64) {
        let mut changes = self.revocations.subscribe();
        while *changes.borrow_and_update() == epoch {
            if changes.changed().await.is_err() {
                return;
            }
        }
    }

    fn revoke(&self, state: &mut State, reason: &str) {
        if !state.unavailable {
            state.unavailable = true;
            self.unavailable.store(true, Ordering::Release);
            self.bump_descriptor_revision();
            self.epoch.fetch_add(1, Ordering::Release);
            self.revocations.send_modify(|epoch| *epoch += 1);
            tracing::warn!(event = "relay_service_unavailable", reason);
        }
    }
}

pub(crate) fn now() -> i64 {
    i64::try_from(
        SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .unwrap_or_default()
            .as_secs(),
    )
    .unwrap_or(i64::MAX)
}

#[cfg(test)]
#[path = "runtime_tests.rs"]
mod tests;

#[cfg(test)]
#[path = "metered_tests.rs"]
mod metered_tests;
