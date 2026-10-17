//! Canonical authentication and frozen time isolate persistence from client rollout.
use base64::{Engine, engine::general_purpose::STANDARD};
use std::{num::NonZeroU64, time::Duration};
use windshare_relay_core::{
    challenge::ChallengeSeed,
    clock::UnixMillis,
    flow::Counts,
    identifiers::RelayIdentity,
    lifecycle::{ConnectionRef, Incarnation},
    registration::RegisterInit,
    sessions::OpaqueHeader,
    share::{CheckpointBudget, Identity, Limits},
};

pub const SENDER: u64 = 1;
pub const RECEIVER: u64 = 2;
pub const WINDOW_FRAMES: u64 = 8;
pub const ATTACHMENT_BYTES: usize = 16_384;
pub const ENVELOPE_BYTES: usize = 512;
const CONNECTION_LIMIT: usize = 4;
const SESSION_LIMIT: usize = 2;
const CONNECTION_FRAMES: u64 = 16;
const REGISTRATION_BUDGET_MS: u64 = 5_000;
const SETTLEMENT_BUDGET_MS: u64 = 5_000;

pub struct Fixture(serde_json::Value);

impl Fixture {
    pub fn new() -> Self {
        let vectors: serde_json::Value = serde_json::from_str(include_str!(
            "../../../../../core/testvectors/v2-session.json"
        ))
        .expect("canonical vectors");
        Self(
            vectors["cases"]
                .as_array()
                .unwrap()
                .iter()
                .find(|case| case["name"] == "relay-capacity-registration")
                .unwrap()
                .clone(),
        )
    }

    pub fn frame(&self, name: &str) -> Vec<u8> {
        STANDARD.decode(self.0[name].as_str().unwrap()).unwrap()
    }

    pub fn now(&self) -> UnixMillis {
        UnixMillis(
            self.0["expiresAt"]
                .as_str()
                .unwrap()
                .parse::<u64>()
                .unwrap()
                * 1000
                - REGISTRATION_BUDGET_MS
                - 1,
        )
    }

    pub fn identity(&self, incarnation: [u8; 16]) -> Identity {
        let init = RegisterInit::decode(&self.frame("registerInitB64")).unwrap();
        Identity {
            relay: RelayIdentity::new(self.frame("relayIdentityB64").try_into().unwrap()).unwrap(),
            share: init.registration().route().share_id(),
            incarnation: Incarnation::new(incarnation).unwrap(),
        }
    }

    pub fn seed(&self) -> ChallengeSeed {
        ChallengeSeed::new(
            self.frame("challengeIdB64").try_into().unwrap(),
            self.frame("challengeNonceB64").try_into().unwrap(),
        )
        .unwrap()
    }
}

pub fn connection(id: u64) -> ConnectionRef {
    ConnectionRef {
        id: NonZeroU64::new(id).unwrap(),
        generation: NonZeroU64::new(1).unwrap(),
    }
}

pub fn limits() -> Limits {
    let capacity =
        |frames| Counts::new(frames, frames * u64::from(OpaqueHeader::MAX_ENCODED_BYTES));
    Limits {
        connections: CONNECTION_LIMIT,
        sessions: SESSION_LIMIT,
        delivery: windshare_relay_core::flow::Limits::new(
            capacity(CONNECTION_FRAMES),
            capacity(2),
            capacity(1),
            Duration::from_millis(SETTLEMENT_BUDGET_MS),
        )
        .unwrap(),
    }
}

pub fn budget() -> CheckpointBudget {
    CheckpointBudget::new(ATTACHMENT_BYTES, ENVELOPE_BYTES).unwrap()
}
