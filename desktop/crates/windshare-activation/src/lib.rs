//! Native entry points carry intent; only the application owns shares.
mod intent;
pub mod launch;
pub mod protocol;
#[cfg(windows)]
pub mod windows;

pub use intent::{EntrySource, IntentId, MAX_SELECTED_ROOTS, ShareIntent};
pub use protocol::{Activation, Phase, Reply, Request};

#[cfg(test)]
#[path = "intent_tests.rs"]
mod tests;
