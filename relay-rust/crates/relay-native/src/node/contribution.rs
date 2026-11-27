use super::{ContributionLimits, Settings, settings::Ceiling};
use crate::Database;
use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize};
use std::sync::{Arc, RwLock};

const DEFAULT_DAILY_BYTES: u64 = 10 * 1024 * 1024 * 1024;
const DEFAULT_PUBLIC_RATE: u64 = 3_125_000;
const DEFAULT_PUBLIC_CONNECTIONS: usize = 64;

#[derive(Clone, Copy, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Policy {
    pub enabled: bool,
    pub daily_bytes: u64,
    pub bytes_per_second: u64,
    pub connections: usize,
    pub host_uplink: Option<u64>,
}
impl Default for Policy {
    fn default() -> Self {
        Self {
            enabled: true,
            daily_bytes: DEFAULT_DAILY_BYTES,
            bytes_per_second: DEFAULT_PUBLIC_RATE,
            connections: DEFAULT_PUBLIC_CONNECTIONS,
            host_uplink: None,
        }
    }
}
impl Policy {
    pub fn validate(&self) -> Result<()> {
        ensure!(
            i64::try_from(self.daily_bytes).is_ok()
                && self.bytes_per_second > 0
                && self.connections > 0
                && self.host_uplink != Some(0),
            "invalid contribution limits"
        );
        Ok(())
    }

    pub(super) fn apply_to(self, settings: &mut Settings) {
        let limit = settings.ceiling;
        settings.contribution = ContributionLimits {
            daily_bytes: Some(limit.daily_bytes.unwrap_or(u64::MAX).min(self.daily_bytes)),
            bytes_per_second: Some(
                limit
                    .bytes_per_second
                    .unwrap_or(u64::MAX)
                    .min(self.bytes_per_second),
            ),
        };
        settings.host_uplink = self.host_uplink;
        settings.access.contributing = self.enabled;
        settings.access.public_connections = limit
            .connections
            .unwrap_or(usize::MAX)
            .min(self.connections);
    }
}

#[derive(Clone)]
pub struct Control {
    store: Database,
    ceiling: Arc<RwLock<Ceiling>>,
}
impl Control {
    pub fn new(store: Database) -> Self {
        Self {
            store,
            ceiling: Arc::default(),
        }
    }
    pub fn view(&self) -> Result<serde_json::Value> {
        let policy = self.store.contribution_policy()?;
        let ceiling = *self.ceiling.read().expect("contribution ceiling");
        Ok(serde_json::json!({"policy": policy, "ceiling": ceiling}))
    }
    pub fn apply(&self, settings: &mut Settings) -> Result<()> {
        let policy = self.store.contribution_policy()?;
        policy.validate()?;
        policy.apply_to(settings);
        Ok(())
    }
    pub(super) fn record_ceiling(&self, ceiling: Ceiling) {
        *self.ceiling.write().expect("contribution ceiling") = ceiling;
    }
}
