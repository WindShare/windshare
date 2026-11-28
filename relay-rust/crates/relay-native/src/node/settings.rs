use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize};
use std::path::Path;
use windshare_relay_access::trust::Generation;

const SERVICE_RELAY_BIT: u8 = 1 << 0;
const SERVICE_STUN_BIT: u8 = 1 << 1;

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Services {
    pub relay: bool,
    pub stun: bool,
}

impl Services {
    pub(super) const fn to_bits(self) -> u8 {
        let mut bits = 0;
        if self.relay {
            bits |= SERVICE_RELAY_BIT;
        }
        if self.stun {
            bits |= SERVICE_STUN_BIT;
        }
        bits
    }

    pub(super) const fn from_bits(bits: u8) -> Self {
        Self {
            relay: bits & SERVICE_RELAY_BIT != 0,
            stun: bits & SERVICE_STUN_BIT != 0,
        }
    }
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

#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Ceiling {
    pub daily_bytes: Option<u64>,
    pub bytes_per_second: Option<u64>,
    pub connections: Option<usize>,
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
    pub operator: String,
    #[serde(default)]
    pub provider: String,
    #[serde(default)]
    pub bootstrap_nodes: Vec<String>,
    #[serde(default)]
    pub ceiling: Ceiling,
    #[serde(skip)]
    pub host_uplink: Option<u64>,
    #[serde(default)]
    pub stun_urls: Vec<String>,
    #[serde(default)]
    pub access: super::access::Policy,
    pub services: Services,
    pub relay_budget: Option<MonthlyBudget>,
    #[serde(skip)]
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
            operator: String::new(),
            provider: String::new(),
            bootstrap_nodes: Vec::new(),
            ceiling: Ceiling::default(),
            host_uplink: None,
            stun_urls: Vec::new(),
            access: super::access::Policy::default(),
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
            self.ceiling.bytes_per_second != Some(0)
                && self.ceiling.connections != Some(0)
                && self
                    .ceiling
                    .daily_bytes
                    .is_none_or(|n| i64::try_from(n).is_ok()),
            "invalid public contribution ceiling"
        );
        ensure!(
            self.stun_urls.len() <= windshare_relay_access::descriptor::MAX_STUN_URLS,
            "too many advertised STUN URLs"
        );
        crate::discovery::Discovery::validate(&self.bootstrap_nodes)?;
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
            budget.validate()?;
        }
        self.access.generations()
    }
}
