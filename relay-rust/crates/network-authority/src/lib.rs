//! Account and traffic policy stays synchronous and independent of the Worker host.
mod account_admin;
mod accounts;
mod administrator;
mod admission;
mod challenges;
#[cfg(any(test, target_arch = "wasm32"))]
mod device_login;
pub mod fleet;
mod governance;
mod model;
mod nodes;
mod password;
mod profile;
mod public_blocks;
mod registration;
mod restriction_store;
mod signing;
#[cfg(any(test, target_arch = "wasm32"))]
mod source;
mod store;
mod usage;
pub use account_admin::{AccountDetails, AccountPage, AccountSummary, DailyUsage};
use anyhow::Result;
pub use challenges::{EmailDelivery, ProofKind};
#[cfg(any(test, target_arch = "wasm32"))]
pub use device_login::DeviceApproval;
pub use governance::{Block, MilestoneReview, Report, ReportPage, ReportPosition};
pub use model::*;
pub use nodes::{Node, NodeDetails, NodeLoadReport, NodePage, NodePolicyChange, Zone};
pub use password::{
    HashedPassword, PASSWORD_SALT_BYTES, PasswordChallenge, PasswordChangePreparation,
    PasswordRegistrationPreparation, PasswordRegistrationPrepare, valid_login_password_length,
    valid_password_length,
};
pub use profile::{EmailVerification, Mailbox};
pub use signing::Signing;
pub use store::{Environment, Store};

pub struct Authority<R, E> {
    pub store: R,
    pub environment: E,
    pub signing: Signing,
    live_loads: std::collections::BTreeMap<[u8; 32], NodeLoadReport>,
}
impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Configuration cannot move the certified signing generation backwards.
    pub fn new(mut store: R, environment: E, signing: Signing) -> Result<Self> {
        signing.activate(&mut store, environment.now())?;
        Ok(Self {
            store,
            environment,
            signing,
            live_loads: std::collections::BTreeMap::new(),
        })
    }

    #[cfg(target_arch = "wasm32")]
    pub(crate) fn set_live_loads(
        &mut self,
        loads: std::collections::BTreeMap<[u8; 32], NodeLoadReport>,
    ) {
        self.live_loads = loads;
    }

    #[cfg(target_arch = "wasm32")]
    pub(crate) fn into_live_loads(self) -> std::collections::BTreeMap<[u8; 32], NodeLoadReport> {
        self.live_loads
    }
    /// # Errors
    /// Propagates malformed or unreadable persisted configuration.
    pub fn settings(&self) -> Result<Settings> {
        Ok(store::load(&self.store, "settings")?.unwrap_or_default())
    }
}
#[cfg(target_arch = "wasm32")]
mod host;
#[cfg(target_arch = "wasm32")]
pub use host::NetworkAuthority;
#[cfg(all(test, not(target_arch = "wasm32")))]
#[path = "host/fleet/reconcile.rs"]
mod directory_reconcile;
#[cfg(all(test, not(target_arch = "wasm32")))]
#[path = "host/telemetry.rs"]
mod telemetry;
#[cfg(test)]
#[path = "authority_tests.rs"]
mod tests;
