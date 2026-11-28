//! Distinct signing purposes keep cached peer exchanges out of live authority checks.
use serde::{Deserialize, Serialize};

pub const DESCRIPTOR_VERSION: u8 = 2;
pub const NODE_PLATFORM_NATIVE: &str = "native";
pub const NODE_IDENTITY_DOMAIN: &[u8] = b"windshare/node-identity/v1\0";
pub const MAX_STUN_URLS: usize = 4;

#[derive(Clone, Copy)]
pub enum Purpose {
    Live,
    Gossip,
}

impl Purpose {
    pub const fn domain(self) -> &'static [u8] {
        match self {
            Self::Live => b"windshare/node-descriptor/v2\0",
            Self::Gossip => b"windshare/node-gossip/v1\0",
        }
    }

    pub const fn lifetime(self) -> u64 {
        match self {
            Self::Live => 300,
            Self::Gossip => 24 * 60 * 60,
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "state", rename_all = "snake_case", deny_unknown_fields)]
pub enum PublicRelay {
    Enabled { anonymous: bool },
    BudgetExhausted,
    Unavailable,
    Disabled,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum InvariantError {
    EnabledWithoutRelayService,
    TooManyStunUrls,
    StunServiceWithoutUrls,
}

/// Validates cross-field service and public-relay invariants shared across
/// node discovery and fleet inspection.
///
/// # Errors
/// Rejects enabled public relay without the relay service or inconsistent STUN counts.
pub const fn validate_service_invariants(
    relay_service: bool,
    stun_service: bool,
    stun_url_count: usize,
    public_relay: PublicRelay,
) -> Result<(), InvariantError> {
    if matches!(public_relay, PublicRelay::Enabled { .. }) && !relay_service {
        return Err(InvariantError::EnabledWithoutRelayService);
    }
    if stun_url_count > MAX_STUN_URLS {
        return Err(InvariantError::TooManyStunUrls);
    }
    if stun_service && stun_url_count == 0 {
        return Err(InvariantError::StunServiceWithoutUrls);
    }
    Ok(())
}

/// Reports whether a node belongs in community peer discovery.
///
/// Only self-hosted nodes currently offering anonymous relaying or STUN qualify;
/// explicit owner opt-out (`Disabled`) and authority-managed official nodes stay
/// outside the community peer pool.
pub const fn offers_community_service(
    has_authority: bool,
    relay_service: bool,
    stun_service: bool,
    public_relay: PublicRelay,
) -> bool {
    !has_authority
        && !matches!(public_relay, PublicRelay::Disabled)
        && ((matches!(public_relay, PublicRelay::Enabled { anonymous: true }) && relay_service)
            || stun_service)
}
