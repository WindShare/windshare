//! Cloudflare relay adapter. Native tests inject I/O into the same host logic.
#[cfg(any(target_arch = "wasm32", test))]
mod config;
#[cfg(any(target_arch = "wasm32", test))]
mod persistence;
#[cfg(any(target_arch = "wasm32", test))]
mod runtime;

#[cfg(target_arch = "wasm32")]
mod platform;
#[cfg(target_arch = "wasm32")]
pub use platform::ShareRelayObject;

#[cfg(test)]
#[path = "runtime_tests.rs"]
mod tests;
