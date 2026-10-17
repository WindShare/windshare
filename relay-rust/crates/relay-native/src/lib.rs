//! Native control-plane host. JOIN and payload forwarding await the coordinated
//! delivery-runtime cutover; this host never accepts or submits opaque payloads.

mod actor;
mod config;
mod environment;
mod registry;
mod socket;
mod storage;

pub use config::Endpoint;
pub use registry::Server;
pub use storage::Database;

#[cfg(test)]
#[path = "fixtures_tests.rs"]
mod test_support;
