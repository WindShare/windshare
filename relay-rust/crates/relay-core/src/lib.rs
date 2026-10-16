//! Shared relay control semantics for native and Cloudflare hosts.
//!
//! Hosts own clocks, persistence, sockets, and opaque payload forwarding so that
//! platform limits and I/O mechanisms cannot become implicit control decisions.
//! This crate decodes the control frames a relay receives, encodes the ones it
//! sends, drives connection handshakes and single-share fresh registration, and
//! turns sender proofs into move-only authorities. Publication remains hidden
//! until the host confirms persistence and connection preparation.

#![no_std]

pub mod authentication;
pub mod challenge;
pub mod clock;
pub mod descriptor_upload;
pub mod error_frame;
pub mod handshake;
pub mod identifiers;
pub mod join;
pub mod lifecycle;
pub mod registration;
pub mod sender_auth;
pub mod stop;
mod wire;

pub use wire::{FrameError, WIRE_VERSION};

#[cfg(test)]
mod test_vectors;

#[cfg(test)]
#[path = "control_vectors_tests.rs"]
mod control_vectors_tests;
