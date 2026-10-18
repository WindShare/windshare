use anyhow::{Result, ensure};

const DEFAULT_BUSINESS: usize = 256;
const DEFAULT_CONTROLS: usize = 32;
const DEFAULT_BUSINESS_PER_SOURCE: usize = 128;
const DEFAULT_CONTROLS_PER_SOURCE: usize = 8;

/// Concurrent relay resources, charged until the last transport owner is destroyed.
#[derive(Clone, Copy, Debug)]
pub struct ConnectionLimits {
    pub business: usize,
    pub controls: usize,
    pub business_per_source: usize,
    pub controls_per_source: usize,
}

impl Default for ConnectionLimits {
    fn default() -> Self {
        Self {
            business: DEFAULT_BUSINESS,
            controls: DEFAULT_CONTROLS,
            business_per_source: DEFAULT_BUSINESS_PER_SOURCE,
            controls_per_source: DEFAULT_CONTROLS_PER_SOURCE,
        }
    }
}

impl ConnectionLimits {
    /// # Errors
    /// Rejects zero capacity and source budgets larger than their global budget.
    pub fn validate(self) -> Result<()> {
        ensure!(
            self.business_per_source != 0 && self.business_per_source <= self.business,
            "business connection limits require 0 < per-source <= global"
        );
        ensure!(
            self.controls_per_source != 0 && self.controls_per_source <= self.controls,
            "control connection limits require 0 < per-source <= global"
        );
        Ok(())
    }
}
