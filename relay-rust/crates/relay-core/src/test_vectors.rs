//! Readers for the cross-runtime vectors that Go generates and every client
//! decodes, so Rust relay semantics are checked against the same bytes.

extern crate std;

use std::{string::String, vec::Vec};

use base64::{Engine, engine::general_purpose::STANDARD};
use serde::de::DeserializeOwned;

use crate::{
    challenge::{Challenge, ChallengePurpose, ChallengeSeed},
    clock::{UnixMillis, UnixSeconds},
    identifiers::{ChallengeId, ChallengeNonce, RelayIdentity},
};

const SESSION_VECTORS: &str = include_str!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/../../../core/testvectors/v2-session.json"
));

/// Returns the named `v2-session.json` case.
pub fn session_case<T: DeserializeOwned>(name: &str) -> T {
    let file: serde_json::Value =
        serde_json::from_str(SESSION_VECTORS).expect("v2-session.json must parse");
    let case = file["cases"]
        .as_array()
        .expect("v2-session.json must list cases")
        .iter()
        .find(|case| case["name"] == name)
        .unwrap_or_else(|| panic!("v2-session.json must contain {name}"));
    serde_json::from_value(case.clone()).unwrap_or_else(|error| panic!("{name}: {error}"))
}

pub fn bytes(encoded: &str) -> Vec<u8> {
    STANDARD
        .decode(encoded)
        .expect("vector bytes must be padded base64")
}

pub fn array<const N: usize>(encoded: &str) -> [u8; N] {
    bytes(encoded)
        .try_into()
        .unwrap_or_else(|value: Vec<u8>| panic!("expected {N} bytes, got {}", value.len()))
}

pub fn unix_seconds(decimal: &str) -> UnixSeconds {
    UnixSeconds(decimal.parse().expect("vector times are decimal strings"))
}

pub fn relay_identity(encoded: &str) -> RelayIdentity {
    RelayIdentity::new(array(encoded)).expect("vector relay identity is nonzero")
}

/// Rebuilds a relay-issued challenge from the fields a vector records.
pub fn challenge(purpose: ChallengePurpose, id: &str, nonce: &str, expires_at: &str) -> Challenge {
    Challenge::new(
        purpose,
        ChallengeId::new(array(id)).expect("vector challenge ID is nonzero"),
        ChallengeNonce::new(array(nonce)).expect("vector challenge nonce is nonzero"),
        unix_seconds(expires_at),
    )
    .expect("vector challenge expiry is nonzero")
}

/// The random values a host would draw to reissue a vector's challenge.
pub fn challenge_seed(id: &str, nonce: &str) -> ChallengeSeed {
    ChallengeSeed::new(array(id), array(nonce)).expect("vector challenge values are nonzero")
}

/// One millisecond before expiry: the last moment a proof is still accepted.
pub const fn last_live_instant(challenge: &Challenge) -> UnixMillis {
    UnixMillis(challenge.expires_at().start().0 - 1)
}

/// A decimal string, as vectors encode integers beyond JavaScript's safe range.
pub type Decimal = String;
