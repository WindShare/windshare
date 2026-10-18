use anyhow::{Result, ensure};
use std::{
    collections::HashSet,
    net::{Ipv4Addr, Ipv6Addr, SocketAddr},
    num::{NonZeroU32, NonZeroUsize},
};

pub(super) const MAX_DATAGRAM_BYTES: usize = 1_200;
const DEFAULT_PORT: u16 = 3478;
const DEFAULT_ADMIN_PORT: u16 = 8081;
const DEFAULT_REQUESTS_PER_SECOND: u32 = 1_000;
const DEFAULT_SOURCE_REQUESTS_PER_SECOND: u32 = 20;
const DEFAULT_MAX_SOURCES: usize = 4_096;
const MAX_LISTENERS: usize = 16;

/// Independent, per-listener fixed-window budgets. Zero never means unlimited.
#[derive(Clone, Copy, Debug)]
pub struct Limits {
    pub requests_per_second: NonZeroU32,
    pub source_requests_per_second: NonZeroU32,
    pub max_sources: NonZeroUsize,
}

impl Default for Limits {
    fn default() -> Self {
        Self {
            requests_per_second: NonZeroU32::new(DEFAULT_REQUESTS_PER_SECOND).unwrap(),
            source_requests_per_second: NonZeroU32::new(DEFAULT_SOURCE_REQUESTS_PER_SECOND)
                .unwrap(),
            max_sources: NonZeroUsize::new(DEFAULT_MAX_SOURCES).unwrap(),
        }
    }
}

/// Numeric socket addresses avoid DNS work in listener startup and recovery.
#[derive(Clone, Debug)]
pub struct Config {
    pub udp_addresses: Vec<SocketAddr>,
    pub admin_address: Option<SocketAddr>,
    pub limits: Limits,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            udp_addresses: vec![
                (Ipv4Addr::UNSPECIFIED, DEFAULT_PORT).into(),
                (Ipv6Addr::UNSPECIFIED, DEFAULT_PORT).into(),
            ],
            admin_address: Some((Ipv4Addr::LOCALHOST, DEFAULT_ADMIN_PORT).into()),
            limits: Limits::default(),
        }
    }
}

impl Config {
    /// # Errors
    /// Rejects excessive or repeated listeners before any sockets are opened.
    pub fn validate(&self) -> Result<()> {
        ensure!(
            self.udp_addresses.len() <= MAX_LISTENERS,
            "too many STUN listeners"
        );
        let mut addresses = HashSet::new();
        for address in &self.udp_addresses {
            ensure!(
                addresses.insert(address),
                "duplicate STUN listener: {address}"
            );
        }
        Ok(())
    }
}

#[cfg(test)]
#[path = "config_tests.rs"]
mod tests;
