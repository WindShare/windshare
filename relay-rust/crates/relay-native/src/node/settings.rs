use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize};
use std::path::Path;

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

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct MonthlyBudget {
    pub bytes: u64,
    pub reset_day_utc: u8,
}

#[derive(Clone, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Settings {
    pub services: Services,
    pub relay_budget: Option<MonthlyBudget>,
}

impl Settings {
    /// # Errors
    /// Refuses malformed configuration rather than enabling services implicitly.
    pub fn read(path: &Path) -> Result<Self> {
        let settings: Self = serde_json::from_slice(&std::fs::read(path)?)?;
        settings.validate()?;
        Ok(settings)
    }

    pub(super) fn validate(&self) -> Result<()> {
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
        Ok(())
    }
}
