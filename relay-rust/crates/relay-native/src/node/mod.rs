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
mod descriptor;
mod reload;
mod settings;
mod trace;
pub use configuration::Configuration;

pub(crate) use metered::Metered;
pub(crate) use metrics::Metrics;
pub use metrics::Snapshot;
pub use settings::{MonthlyBudget, Services, Settings};
pub(crate) use trace::NodeTrace;

use crate::storage::Database;
use anyhow::{Result, anyhow};
use std::{
    sync::{Arc, Mutex, MutexGuard, OnceLock},
    time::{SystemTime, UNIX_EPOCH},
};
use tokio::sync::watch;

struct State {
    settings: Settings,
    identity: Option<Identity>,
    control: Option<Database>,
    budget: Option<budget::Budget>,
    unavailable: bool,
    storage_failed: bool,
    budget_revision: u64,
}

#[derive(Clone, Copy, PartialEq, Eq)]
pub(crate) struct BudgetEpoch {
    period: i64,
    revision: u64,
}

pub(crate) struct Grant {
    epoch: BudgetEpoch,
    bytes: u64,
}

impl State {
    fn budget_epoch(&self) -> BudgetEpoch {
        BudgetEpoch {
            period: self.budget.as_ref().map_or(0, budget::Budget::period),
            revision: self.budget_revision,
        }
    }
}

#[derive(Clone)]
pub struct Runtime {
    pub(crate) access: access::Service,
    pub(crate) console: Arc<OnceLock<crate::console::Console>>,
    pub(crate) governor: governor::Governor,
    state: Arc<Mutex<State>>,
    control_writer: Arc<Mutex<()>>,
    reporting_authority: Arc<OnceLock<Option<usage::Authority>>>,
    revocations: watch::Sender<u64>,
    pub(crate) metrics: Metrics,
    pub(crate) usage: usage::Collector,
}

impl Default for Runtime {
    fn default() -> Self {
        let access = access::Service::default();
        Self {
            governor: governor::Governor::new(access.clone()),
            access,
            console: Arc::default(),
            state: Arc::new(Mutex::new(State {
                settings: Settings::default(),
                identity: None,
                control: None,
                budget: None,
                unavailable: false,
                storage_failed: false,
                budget_revision: 0,
            })),
            control_writer: Arc::default(),
            reporting_authority: Arc::default(),
            revocations: watch::channel(0).0,
            metrics: Metrics::default(),
            usage: usage::Collector::default(),
        }
    }
}

impl Runtime {
    #[cfg(test)]
    pub(crate) fn new(settings: Settings, store: Database) -> Result<Self> {
        let runtime = Self::default();
        runtime.configure(settings, store)?;
        Ok(runtime)
    }

    pub(crate) fn configure(&self, settings: Settings, store: Database) -> Result<()> {
        let generations = settings.validate()?;
        let writer = self.lock_control()?;
        let timestamp = now();
        let access_now = timestamp.unsigned_abs();
        self.access
            .restore_access(store.initialize_control(access_now)?, access_now)?;
        let budget = budget::Budget::new(store.clone())?;
        let mut state = self.state.lock().map_err(|_| anyhow!("node state"))?;
        state.control = Some(store);
        state.budget = Some(budget);
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
        self.state.lock().map_or(
            Services {
                relay: false,
                stun: false,
            },
            |state| state.settings.services,
        )
    }

    pub fn available(&self) -> bool {
        self.state.lock().is_ok_and(|state| !state.unavailable)
    }

    pub fn snapshot(&self) -> Snapshot {
        self.metrics.snapshot()
    }

    pub(crate) fn epoch(&self) -> u64 {
        *self.revocations.borrow()
    }

    pub(crate) fn budget_epoch(&self) -> BudgetEpoch {
        self.state.lock().expect("node state").budget_epoch()
    }

    pub(crate) fn try_reserve(&self, requested: u64) -> Option<Grant> {
        let mut state = self.state.lock().expect("node state");
        if state.unavailable {
            return Some(Grant {
                epoch: state.budget_epoch(),
                bytes: 0,
            });
        }
        let granted = state
            .settings
            .relay_budget
            .map_or(Some(requested), |config| {
                state
                    .budget
                    .as_mut()
                    .and_then(|budget| budget.try_reserve(config.bytes, requested))
            });
        if granted == Some(0) {
            self.revoke(&mut state, "budget_exhausted");
        }
        let epoch = state.budget_epoch();
        drop(state);
        granted.map(|bytes| Grant { epoch, bytes })
    }

    pub(crate) async fn revoked(&self, epoch: u64) {
        let mut changes = self.revocations.subscribe();
        while *changes.borrow_and_update() == epoch {
            if changes.changed().await.is_err() {
                return;
            }
        }
    }

    pub(crate) fn reserve_for_period(&self, requested: u64) -> Result<Grant> {
        let writer = self.lock_control()?;
        let mut state = self
            .state
            .lock()
            .map_err(|_| anyhow!("node state poisoned"))?;
        let epoch = state.budget_epoch();
        if state.unavailable {
            return Ok(Grant { epoch, bytes: 0 });
        }
        let result = if let Some(config) = state.settings.relay_budget {
            state
                .budget
                .as_mut()
                .ok_or_else(|| anyhow!("missing persistent relay ledger"))?
                .reserve(config.bytes, requested)
        } else {
            Ok(requested)
        };
        state.storage_failed = result.is_err();
        if !result.as_ref().is_ok_and(|granted| *granted > 0) {
            self.revoke(&mut state, "budget_exhausted_or_storage_failure");
        }
        drop(state);
        drop(writer);
        // Pair the grant with the period that was charged, so a pending socket
        // write cannot spend the credit after the ledger advances.
        result.map(|bytes| Grant { epoch, bytes })
    }

    fn revoke(&self, state: &mut State, reason: &str) {
        if !state.unavailable {
            state.unavailable = true;
            self.revocations.send_modify(|epoch| *epoch += 1);
            tracing::warn!(event = "relay_service_unavailable", reason);
        }
    }
}

fn now() -> i64 {
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
