//! Native HTTP/WebSocket lifecycle host with bounded connection I/O.

mod actor;
mod config;
mod connection;
mod environment;
mod generation;
mod http;
mod registry;
mod socket;
mod storage;
pub mod stun;

pub use config::Endpoint;
pub use connection::ConnectionLimits;
pub use http::{OriginPolicy, SourcePolicy};
pub use registry::Server;
pub use storage::Database;

#[cfg(test)]
#[path = "fixtures_tests.rs"]
mod test_support;
