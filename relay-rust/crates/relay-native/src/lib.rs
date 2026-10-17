//! Native lifecycle host with bounded connection I/O. Network JOIN and opaque
//! ingress await delivery recovery and coordinated client runtime integration.

mod actor;
mod config;
mod connection;
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
