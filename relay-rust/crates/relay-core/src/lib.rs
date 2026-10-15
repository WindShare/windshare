//! Shared relay control semantics for native and Cloudflare hosts.
//!
//! Hosts own clocks, persistence, sockets, and opaque payload forwarding so that
//! platform limits and I/O mechanisms cannot become implicit control decisions.

#![no_std]
