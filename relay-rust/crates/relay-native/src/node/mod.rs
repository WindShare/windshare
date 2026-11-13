//! Node authority is shared by admission, discovery and every existing transport.
mod budget;
mod metered;
mod metrics;
mod settings;
mod trace;

pub(crate) use metered::Metered;
pub(crate) use metrics::Metrics;
pub use metrics::Snapshot;
pub use settings::{MonthlyBudget, Services, Settings};
pub(crate) use trace::NodeTrace;

use crate::storage::Database;
use anyhow::{Result, anyhow};
use std::{
    path::PathBuf,
    sync::{Arc, Mutex},
    time::{Duration, SystemTime, UNIX_EPOCH},
};
use tokio::sync::watch;

const SETTINGS_INTERVAL: Duration = Duration::from_secs(1);
const SUMMARY_INTERVAL: Duration = Duration::from_secs(60);

struct State {
    settings: Settings,
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
    state: Arc<Mutex<State>>,
    revocations: watch::Sender<u64>,
    pub(crate) metrics: Metrics,
}

impl Default for Runtime {
    fn default() -> Self {
        Self {
            state: Arc::new(Mutex::new(State {
                settings: Settings::default(),
                budget: None,
                unavailable: false,
                storage_failed: false,
                budget_revision: 0,
            })),
            revocations: watch::channel(0).0,
            metrics: Metrics::default(),
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
        self.state
            .lock()
            .map_err(|_| anyhow!("node state poisoned"))?
            .budget = Some(budget::Budget::new(store)?);
        self.update(settings, now())
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

    pub(crate) fn reserve(&self, requested: u64) -> Result<u64> {
        let mut state = self
            .state
            .lock()
            .map_err(|_| anyhow!("node state poisoned"))?;
        if state.unavailable {
            return Ok(0);
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
        result
    }

    pub(crate) fn reserve_for_period(&self, requested: u64) -> Result<Grant> {
        // The grant carries its accounting period, so an interrupted write cannot
        // spend last month's permission after the ledger has advanced.
        let epoch = self.budget_epoch();
        let bytes = self.reserve(requested)?;
        Ok(Grant { epoch, bytes })
    }

    /// # Errors
    /// Configuration changes cannot reset a billing ledger or revive spent quota.
    pub fn update(&self, settings: Settings, timestamp: i64) -> Result<()> {
        settings.validate()?;
        let mut state = self
            .state
            .lock()
            .map_err(|_| anyhow!("node state poisoned"))?;
        if let Some(config) = settings.relay_budget {
            state
                .budget
                .as_mut()
                .ok_or_else(|| anyhow!("missing persistent relay ledger"))?
                .update(config, timestamp)?;
        }
        let exhausted = settings.relay_budget.is_some_and(|config| {
            state
                .budget
                .as_ref()
                .is_none_or(|budget| budget.remaining(config.bytes) == 0)
        });
        if !settings.services.relay || exhausted || state.storage_failed {
            self.revoke(
                &mut state,
                if exhausted {
                    "budget_exhausted"
                } else {
                    "service_disabled"
                },
            );
        } else if state.unavailable {
            state.unavailable = false;
            tracing::info!(event = "relay_service_available", timestamp);
        }
        if state.settings != settings {
            tracing::info!(event = "node_settings_changed", ?settings, timestamp);
        }
        if state.settings.relay_budget != settings.relay_budget {
            // Pending socket writes must not reuse unlimited credit after a
            // budget is enabled or reduced, even within the same billing period.
            state.budget_revision += 1;
        }
        state.settings = settings;
        drop(state);
        Ok(())
    }

    fn revoke(&self, state: &mut State, reason: &str) {
        if !state.unavailable {
            state.unavailable = true;
            self.revocations.send_modify(|epoch| *epoch += 1);
            tracing::warn!(event = "relay_service_unavailable", reason);
        }
    }

    pub(crate) async fn maintain(&self, path: Option<PathBuf>) {
        let mut tick = tokio::time::interval(SETTINGS_INTERVAL);
        let mut summary = std::time::Instant::now();
        loop {
            tick.tick().await;
            if summary.elapsed() >= SUMMARY_INTERVAL {
                let snapshot = self.snapshot();
                tracing::info!(event = "node_load_summary", summary = ?snapshot);
                summary = std::time::Instant::now();
            }
            let runtime = self.clone();
            let path = path.clone();
            let result = tokio::task::spawn_blocking(move || {
                let settings = if let Some(path) = path {
                    Settings::read(&path)?
                } else {
                    runtime
                        .state
                        .lock()
                        .map_err(|_| anyhow!("node state poisoned"))?
                        .settings
                        .clone()
                };
                runtime.update(settings, now())
            })
            .await;
            if !matches!(result, Ok(Ok(()))) {
                tracing::warn!(event = "node_settings_refresh_failed", ?result);
            }
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
