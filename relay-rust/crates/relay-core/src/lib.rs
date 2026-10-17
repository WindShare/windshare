//! Shared relay semantics for native and Cloudflare hosts, using `no_std + alloc`.
//!
//! [`share::ShareRelay`] owns each share's lifecycle, handshakes, sessions, delivery
//! ledgers and deadlines. It emits ordered I/O obligations and consumes exact
//! completions, keeping publication and irreversible STOP barriers in one place.
//! Its synchronous submission boundary rechecks authority and charges delivery
//! before host handoff; retirement cannot refund bytes still held by a transport.
//!
//! Hosts provide time, entropy, capacity and I/O. Opaque payloads never enter the
//! engine. Aggregate checkpoints restore surviving authority and outstanding
//! delivery together; lower-level state machines remain independently testable.
//! All decisions use [`observation::Observer`].

#![no_std]

extern crate alloc;

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
pub mod observation;
pub mod registration;
pub mod sender_auth;
pub mod sessions;
pub mod share;
pub mod stop;
mod wire;

pub use wire::{FrameError, WIRE_VERSION};

#[cfg(test)]
mod test_vectors;

#[cfg(test)]
#[path = "control_vectors_tests.rs"]
mod control_vectors_tests;
