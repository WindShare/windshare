//! Account and traffic policy stays synchronous and independent of the Worker host.
mod accounts;
mod administrator;
mod admission;
mod challenges;
#[cfg(any(test, target_arch = "wasm32"))]
mod device_login;
mod governance;
mod model;
mod nodes;
mod profile;
mod public_blocks;
mod registration;
mod restriction_store;
mod signing;
#[cfg(any(test, target_arch = "wasm32"))]
mod source;
mod store;
mod usage;
use anyhow::Result;
pub use challenges::{EmailDelivery, ProofKind};
pub use governance::{Block, Report, ReportPage, ReportPosition};
pub use model::*;
pub use nodes::{
    Node, NodeDetails, NodeEnrollment, NodeLoadReport, NodePage, NodePolicyChange, Zone,
};
pub use profile::{EmailVerification, Mailbox};
pub use signing::Signing;
pub use store::{Environment, Store};

pub struct Authority<R, E> {
    pub store: R,
    pub environment: E,
    pub signing: Signing,
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
        })
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
#[cfg(test)]
#[path = "authority_tests.rs"]
mod tests;
