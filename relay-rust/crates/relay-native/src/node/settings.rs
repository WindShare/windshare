use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize};
use std::path::Path;
use windshare_relay_access::trust::Generation;

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Services {
    pub relay: bool,
    pub stun: bool,
}

impl Default for Services {
    fn default() -> Self {
        Self {
            relay: true,
            stun: true,
        }
    }
}

pub use windshare_relay_access::usage::MonthlyBudget;

#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ContributionLimits {
    pub daily_bytes: Option<u64>,
    pub bytes_per_second: Option<u64>,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Settings {
    #[serde(default)]
    pub authority: Option<super::usage::Authority>,
    #[serde(default = "global_zone")]
    pub zone: String,
    #[serde(default)]
    pub region: String,
    #[serde(default)]
    pub stun_urls: Vec<String>,
    #[serde(default)]
    pub access: super::access::Policy,
    #[serde(default)]
    pub capacity_bytes_per_second: Option<u64>,
    pub services: Services,
    pub relay_budget: Option<MonthlyBudget>,
    #[serde(default)]
    pub contribution: ContributionLimits,
}

pub(super) fn zone_defaults(node: &mut serde_json::Value) {
    if node.get("zone").and_then(serde_json::Value::as_str) == Some("cn") {
        let access = node
            .as_object_mut()
            .expect("object with zone")
            .entry("access")
            .or_insert_with(|| serde_json::json!({}));
        if let Some(access) = access.as_object_mut() {
            access
                .entry("anonymous")
                .or_insert(serde_json::Value::Bool(false));
        }
    }
}
fn global_zone() -> String {
    "global".into()
}
impl Default for Settings {
    fn default() -> Self {
        Self {
            authority: None,
            zone: global_zone(),
            region: String::new(),
            stun_urls: Vec::new(),
            access: super::access::Policy::default(),
            capacity_bytes_per_second: None,
            services: Services::default(),
            relay_budget: None,
            contribution: ContributionLimits::default(),
        }
    }
}

impl Settings {
    /// # Errors
    /// Refuses malformed configuration rather than enabling services implicitly.
    pub fn read(path: &Path) -> Result<Self> {
        Ok(super::Configuration::read(path)?.node)
    }

    pub(super) fn validate(&self) -> Result<Vec<Generation>> {
        ensure!(
            matches!(self.zone.as_str(), "global" | "cn"),
            "invalid node zone"
        );
        if let Some(authority) = &self.authority {
            authority.validate()?;
            ensure!(
                self.access
                    .trusted_issuers
                    .iter()
                    .any(|trusted| trusted.root == authority.issuer),
                "reporting authority must be a trusted issuer"
            );
        }
        ensure!(
            self.capacity_bytes_per_second != Some(0),
            "relay capacity must be positive"
        );
        ensure!(
            self.contribution.bytes_per_second != Some(0),
            "public relay bandwidth must be positive"
        );
        ensure!(
            self.contribution
                .daily_bytes
                .is_none_or(|bytes| i64::try_from(bytes).is_ok()),
            "public daily budget exceeds storage range"
        );
        if let Some(budget) = self.relay_budget {
            ensure!(
                (1..=28).contains(&budget.reset_day_utc),
                "billing reset day must be 1..28 UTC"
            );
            ensure!(
                i64::try_from(budget.bytes).is_ok(),
                "relay budget exceeds storage range"
            );
        }
        self.access.generations()
    }
}
