use super::{Identity, Runtime, Settings, State, access::PreparedPolicy, now, usage::Authority};
use crate::storage::budget::BudgetRecord;
use anyhow::{Context, Result, anyhow, ensure};
use std::{
    path::{Path, PathBuf},
    time::{Duration, Instant},
};
use windshare_relay_access::{Issuer, trust::Generation};

const SETTINGS_INTERVAL: Duration = Duration::from_secs(1);
const SUMMARY_INTERVAL: Duration = Duration::from_secs(60);

struct PreparedSettings<'a> {
    settings: Settings,
    budget: Option<BudgetRecord>,
    controller: Option<Issuer>,
    access: Option<PreparedPolicy<'a>>,
}

impl Runtime {
    /// # Errors
    /// Rejects the whole update if configuration validation or persistence fails.
    pub fn update(&self, settings: Settings, timestamp: i64) -> Result<()> {
        let generations = settings.validate()?;
        let writer = self.lock_control()?;
        let result = self.apply_settings(settings, &generations, timestamp);
        drop(writer);
        result
    }

    // The caller holds the control writer until publication, so quota reservations
    // and authority responses cannot supersede either half of this transition.
    pub(super) fn apply_settings(
        &self,
        settings: Settings,
        generations: &[Generation],
        timestamp: i64,
    ) -> Result<()> {
        ensure!(
            self.reporting_authority
                .get()
                .is_none_or(|authority| *authority == settings.authority),
            "changing the accounting authority requires a node restart"
        );
        let controller = settings
            .authority
            .as_ref()
            .map(Authority::issuer)
            .transpose()?;

        let state = self
            .state
            .lock()
            .map_err(|_| anyhow!("node state poisoned"))?;
        let budget = if let Some(config) = settings.relay_budget {
            state
                .budget
                .as_ref()
                .ok_or_else(|| anyhow!("missing persistent relay ledger"))?
                .next_record(config, timestamp)?
        } else {
            None
        };
        let owner = state.identity.as_ref().map(Identity::owner);
        let store = state.control.clone();
        drop(state);

        let mut policy = settings.access.clone();
        if let Some(owner) = owner {
            policy.owner_principals.push(owner.principal);
            policy.credentials.push(owner);
        }
        let policy = Some(policy).filter(|policy| !self.access.policy_matches(policy));
        let generations = if policy.is_some() { generations } else { &[] };
        let prepare_access = |records| {
            policy
                .map(|policy| self.access.prepare_policy(policy, records))
                .transpose()
        };
        let access = match store {
            Some(store) => store
                .persist_node_settings(budget, generations, prepare_access)
                .context("persisting node settings")?,
            None => prepare_access(generations.iter().copied().map(Into::into).collect())?,
        };

        self.publish_settings(
            PreparedSettings {
                settings,
                budget,
                controller,
                access,
            },
            timestamp,
        );
        Ok(())
    }

    fn publish_settings(&self, prepared: PreparedSettings<'_>, timestamp: i64) {
        let PreparedSettings {
            settings,
            budget,
            controller,
            access,
        } = prepared;
        let mut state = self.state.lock().expect("node state");
        let changed = state.settings != settings;
        let publication =
            self.governor
                .publish_capacity(settings.capacity_bytes_per_second, || {
                    if let Some(record) = budget {
                        state
                            .budget
                            .as_mut()
                            .expect("prepared relay ledger")
                            .advance(record);
                    }
                    if state.settings.relay_budget != settings.relay_budget {
                        // A pending socket write must not reuse credit from the previous
                        // limit, even when the billing period itself is still current.
                        state.budget_revision += 1;
                    }
                    state.settings = settings;
                    let publication = self.access.publish_policy(controller, access);
                    self.refresh_availability(&mut state, timestamp);
                    publication
                });
        if changed {
            tracing::info!(
                event = "node_settings_changed",
                timestamp,
                zone = state.settings.zone,
                capacity_bytes_per_second = state.settings.capacity_bytes_per_second,
                relay_enabled = state.settings.services.relay,
                stun_enabled = state.settings.services.stun,
                access_changed = publication.changed,
            );
        }
        drop(state);
        if publication.changed {
            self.access.notify_policy_changed();
        }
    }

    fn refresh_availability(&self, state: &mut State, timestamp: i64) {
        let exhausted = state.settings.relay_budget.is_some_and(|config| {
            state
                .budget
                .as_ref()
                .is_none_or(|budget| budget.remaining(config.bytes) == 0)
        });
        if !state.settings.services.relay || exhausted || state.storage_failed {
            self.revoke(
                state,
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
    }

    fn refresh_current_settings(&self, timestamp: i64) -> Result<()> {
        let writer = self.lock_control()?;
        let settings = self
            .state
            .lock()
            .map_err(|_| anyhow!("node state poisoned"))?
            .settings
            .clone();
        let generations = settings.validate()?;
        let result = self.apply_settings(settings, &generations, timestamp);
        drop(writer);
        result
    }

    fn refresh_settings(&self, path: Option<&Path>, timestamp: i64) -> Result<()> {
        let Some(path) = path else {
            return self.refresh_current_settings(timestamp);
        };
        let result = Settings::read(path).and_then(|settings| self.update(settings, timestamp));
        // A rejected file must not freeze the active ledger at the end of its month.
        if result.is_err()
            && let Err(error) = self.refresh_current_settings(timestamp)
        {
            tracing::warn!(event = "node_active_settings_refresh_failed", timestamp, %error);
        }
        result
    }

    pub(crate) async fn maintain(&self, path: Option<PathBuf>) {
        let mut tick = tokio::time::interval(SETTINGS_INTERVAL);
        let mut summary = Instant::now();
        loop {
            tick.tick().await;
            if summary.elapsed() >= SUMMARY_INTERVAL {
                let snapshot = self.snapshot();
                tracing::info!(event = "node_load_summary", summary = ?snapshot);
                summary = Instant::now();
            }
            let runtime = self.clone();
            let path = path.clone();
            let result = tokio::task::spawn_blocking(move || {
                runtime.refresh_settings(path.as_deref(), now())
            })
            .await;
            if !matches!(result, Ok(Ok(()))) {
                tracing::warn!(event = "node_settings_refresh_failed", ?result);
            }
        }
    }
}

#[cfg(test)]
#[path = "reload_tests.rs"]
mod tests;
