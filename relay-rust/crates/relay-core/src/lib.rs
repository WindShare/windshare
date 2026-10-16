//! Shared relay control semantics for native and Cloudflare hosts.
//!
//! Hosts own clocks, persistence, sockets, and opaque payload forwarding so that
//! platform limits and I/O mechanisms cannot become implicit control decisions.
//! This crate decodes the control frames a relay receives, encodes the ones it
//! sends, drives connection handshakes and single-share registration, resume
//! and owner-loss grace, and turns sender proofs into move-only authorities.
//! Irreversible STOP persists its cleanup obligation across restart, then waits
//! for exact retirement and final durable confirmation before replying.
//! Publication remains hidden until the host confirms persistence and
//! connection preparation, and a share lifecycle is constructed only from the
//! durable record its host stores. Receiver sessions keep an absolute admission
//! budget and issue routing permits checked against live session and share
//! authority immediately before submission. Connection delivery ledgers reserve
//! startup and ordinary capacity, retaining submitted occupancy through retirement
//! until consumption, settlement or confirmed closure. Payloads remain host-owned.

#![no_std]

pub mod authentication;
pub mod challenge;
pub mod clock;
pub mod descriptor_upload;
pub mod error_frame;
pub mod flow;
pub mod handshake;
pub mod identifiers;
pub mod join;
pub mod lifecycle;
pub mod registration;
pub mod sender_auth;
pub mod sessions;
pub mod stop;
mod wire;

pub use wire::{FrameError, WIRE_VERSION};

#[cfg(test)]
mod test_vectors;

#[cfg(test)]
#[path = "control_vectors_tests.rs"]
mod control_vectors_tests;
