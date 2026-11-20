use super::{
    Configuration, Identity, Runtime, Settings, State, access::PreparedPolicy, now,
    usage::Authority,
};
use crate::storage::budget::BudgetRecords;
use anyhow::{Context, Result, anyhow, ensure};
use std::{
    path::{Path, PathBuf},
    sync::atomic::Ordering,
    time::{Duration, Instant},
};
use windshare_relay_access::{Issuer, trust::Generation};

const SETTINGS_INTERVAL: Duration = Duration::from_secs(1);
const SUMMARY_INTERVAL: Duration = Duration::from_secs(60);

struct PreparedSettings<'a> {
    settings: Settings,
    budgets: BudgetRecords,
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
        let budgets = state.budgets.rollover(&settings, timestamp)?;
        ensure!(
            budgets.is_empty() || state.control.is_some(),
            "missing persistent relay ledger"
        );
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
                .persist_node_settings(budgets, generations, prepare_access)
                .context("persisting node settings")?,
            None => prepare_access(generations.iter().copied().map(Into::into).collect())?,
        };

        self.publish_settings(
            PreparedSettings {
                settings,
                budgets,
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
            budgets,
            controller,
            access,
        } = prepared;
        let mut state = self.state.lock().expect("node state");
        let changed = state.settings != settings;
        let was_public_exhausted = state.budgets.public_exhausted(&state.settings, timestamp);
        let publication = self.governor.publish_capacity(
            settings.capacity_bytes_per_second,
            settings.contribution.bytes_per_second,
            || {
                state.budgets.advance(budgets);
                state.settings = settings;
                let publication = self.access.publish_policy(controller, access);
                self.refresh_availability(&mut state, timestamp);
                self.has_budgets.store(
                    state.settings.relay_budget.is_some()
                        || state.settings.contribution.daily_bytes.is_some(),
                    Ordering::Release,
                );
                self.next_rollover.store(
                    state.budgets.next_rollover(&state.settings),
                    Ordering::Release,
                );
                publication
            },
        );
        let public_just_exhausted =
            !was_public_exhausted && state.budgets.public_exhausted(&state.settings, timestamp);
        if changed {
            tracing::info!(
                event = "node_settings_changed",
                timestamp,
                zone = state.settings.zone,
                capacity_bytes_per_second = state.settings.capacity_bytes_per_second,
                monthly_relay_bytes = state.settings.relay_budget.map(|budget| budget.bytes),
                public_daily_bytes = state.settings.contribution.daily_bytes,
                public_bytes_per_second = state.settings.contribution.bytes_per_second,
                relay_enabled = state.settings.services.relay,
                stun_enabled = state.settings.services.stun,
                access_changed = publication.changed,
            );
        }
        // Track the active unpaced state independently of `changed` so initial
        // default settings still warn once without repeating on every 1s reload.
        // Public pacing alone still governs non-owner traffic even without a node rate.
        let unpaced_relay = state.settings.services.relay
            && state.settings.capacity_bytes_per_second.is_none()
            && state.settings.contribution.bytes_per_second.is_none();
        let unpaced_warning = (unpaced_relay && !state.unpaced_relay_warned).then(|| {
            (
                state.identity.as_ref().map(Identity::id),
                state.settings.zone.clone(),
            )
        });
        state.unpaced_relay_warned = unpaced_relay;
        drop(state);
        if let Some((node_id, zone)) = unpaced_warning {
            tracing::warn!(
                event = "relay_pacing_disabled",
                timestamp,
                node_id = node_id.as_deref(),
                %zone,
                "relay enabled without capacity_bytes_per_second or contribution.bytes_per_second; fair scheduling and priority pacing are bypassed"
            );
        }
        if publication.changed || public_just_exhausted {
            self.access.notify_policy_changed();
        }
    }

    fn refresh_availability(&self, state: &mut State, timestamp: i64) {
        let exhausted = state.budgets.node_remaining(&state.settings) == 0;
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
            self.unavailable.store(false, Ordering::Release);
            tracing::info!(event = "relay_service_available", timestamp);
        }
    }

    // Rolls over active billing/daily windows without re-verifying issuer
    // certificates or locking the governor when settings have not changed.
    fn rollover_budgets(&self, timestamp: i64) -> Result<()> {
        if timestamp < self.next_rollover.load(Ordering::Acquire) {
            return Ok(());
        }
        let writer = self.lock_control()?;
        let (budgets, store) = {
            let state = self
                .state
                .lock()
                .map_err(|_| anyhow!("node state poisoned"))?;
            let budgets = state.budgets.rollover(&state.settings, timestamp)?;
            if budgets.is_empty() {
                self.next_rollover.store(
                    state.budgets.next_rollover(&state.settings),
                    Ordering::Release,
                );
                return Ok(());
            }
            ensure!(state.control.is_some(), "missing persistent relay ledger");
            (budgets, state.control.clone())
        };
        if let Some(store) = store {
            store
                .persist_node_settings(budgets, &[], |_| Ok(()))
                .context("persisting rolled-over relay budgets")?;
        }
        let mut state = self.state.lock().expect("node state");
        let was_public_exhausted = state.budgets.public_exhausted(&state.settings, timestamp);
        state.budgets.advance(budgets);
        self.refresh_availability(&mut state, timestamp);
        self.next_rollover.store(
            state.budgets.next_rollover(&state.settings),
            Ordering::Release,
        );
        let public_just_exhausted =
            !was_public_exhausted && state.budgets.public_exhausted(&state.settings, timestamp);
        drop(state);
        drop(writer);
        if public_just_exhausted {
            self.access.notify_policy_changed();
        }
        Ok(())
    }

    fn refresh_settings(&self, path: Option<&Path>, timestamp: i64) -> Result<()> {
        let Some(path) = path else {
            return self.rollover_budgets(timestamp);
        };
        let raw = match std::fs::read(path) {
            Ok(raw) => raw,
            Err(error) => {
                if let Err(rollover) = self.rollover_budgets(timestamp) {
                    tracing::warn!(event = "node_active_settings_refresh_failed", timestamp, %rollover);
                }
                return Err(error.into());
            }
        };
        let unchanged = self
            .last_config_bytes
            .lock()
            .expect("config cache")
            .as_deref()
            == Some(raw.as_slice());
        if unchanged {
            return self.rollover_budgets(timestamp);
        }
        let result = Configuration::read_bytes(path, &raw)
            .and_then(|config| self.update(config.node, timestamp));
        if result.is_ok() {
            *self.last_config_bytes.lock().expect("config cache") = Some(raw);
            return Ok(());
        }
        // A rejected file must not freeze the active ledger at the end of its month.
        if let Err(error) = self.rollover_budgets(timestamp) {
            tracing::warn!(event = "node_active_settings_refresh_failed", timestamp, %error);
        }
        result
    }

    pub(crate) async fn maintain(&self, path: Option<PathBuf>) {
        let mut tick = tokio::time::interval(SETTINGS_INTERVAL);
        let mut summary = Instant::now();
        loop {
            tick.tick().await;
            let timestamp = now();
            if summary.elapsed() >= SUMMARY_INTERVAL {
                // Prune expired quota entries periodically even while the governor sleeps idle.
                self.access.expire_quotas(timestamp.unsigned_abs());
                let snapshot = self.snapshot();
                tracing::info!(event = "node_load_summary", summary = ?snapshot);
                summary = Instant::now();
            }
            if path.is_none() && timestamp < self.next_rollover.load(Ordering::Acquire) {
                continue;
            }
            let runtime = self.clone();
            let path = path.clone();
            let result = tokio::task::spawn_blocking(move || {
                runtime.refresh_settings(path.as_deref(), timestamp)
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
