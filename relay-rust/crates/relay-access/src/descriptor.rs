//! Distinct signing purposes keep cached peer exchanges out of live authority checks.
use serde::{Deserialize, Serialize};

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
