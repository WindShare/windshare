use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use sha2::{Digest, Sha256};
use std::time::Duration;
use windshare_relay_core::{
    flow::{self, Counts},
    identifiers::{RelayIdentity, ShareId},
    sessions::OpaqueHeader,
    share::{CheckpointBudget, Limits, RetirementPolicy},
};

pub const WEBSOCKET_PATH: &str = "/v2/ws";
pub const MAX_CONTROL_BYTES: usize =
    windshare_relay_core::descriptor_upload::MAX_DESCRIPTOR_BYTES + 12;
pub const MAX_BUSINESS_CONNECTIONS: usize = 8;
const CONTROL_CONNECTIONS: usize = 8;
const CONNECTION_ATTEMPTS: windshare_relay_core::share::AttemptRate =
    windshare_relay_core::share::AttemptRate {
        burst: 64,
        refill: Duration::from_millis(100),
    };
const RETAINED_FLOWS: usize = 32;
const ATTACHMENT_BYTES: usize = 16_384;
const ATTACHMENT_ENVELOPE_BYTES: usize = 512;
const CONNECTION_FRAMES: u64 = 16;
const SETTLEMENT_TIMEOUT: Duration = Duration::from_secs(5);
// The probes establish no safer physical-buffer TTL. Prefer immediate replacement
// after quiescence; the attempt-rate budget still bounds reconnect admission.
const LOST_GENERATION_RISK_RETENTION: Duration = Duration::ZERO;
const RELAY_IDENTITY_DOMAIN: &[u8] = b"windshare/v2 relay-identity\0";

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Route {
    pub endpoint: String,
    pub relay: RelayIdentity,
    pub share: ShareId,
}

impl Route {
    /// # Errors
    /// Requires exactly one canonical public routing hint; it grants no authority.
    pub fn parse(raw: &str) -> Result<Self> {
        let mut url = url::Url::parse(raw)?;
        ensure!(
            matches!(url.scheme(), "http" | "https"),
            "HTTP upgrade required"
        );
        ensure!(
            url.path() == WEBSOCKET_PATH && url.fragment().is_none(),
            "unknown endpoint"
        );
        ensure!(
            url.username().is_empty() && url.password().is_none(),
            "URL credentials"
        );
        let pairs: Vec<_> = url.query_pairs().collect();
        ensure!(
            pairs.len() == 1 && pairs[0].0 == "share_id",
            "one share_id required"
        );
        let bytes = URL_SAFE_NO_PAD.decode(pairs[0].1.as_bytes())?;
        ensure!(
            URL_SAFE_NO_PAD.encode(&bytes) == pairs[0].1,
            "noncanonical share_id"
        );
        let share = ShareId::new(bytes.try_into().map_err(|_| anyhow!("share_id width"))?)
            .ok_or_else(|| anyhow!("zero share_id"))?;
        drop(pairs);
        let scheme = if url.scheme() == "https" { "wss" } else { "ws" };
        url.set_scheme(scheme)
            .map_err(|()| anyhow!("WebSocket scheme"))?;
        url.set_query(None);
        let endpoint = url.to_string();
        let relay = RelayIdentity::new(
            Sha256::new()
                .chain_update(RELAY_IDENTITY_DOMAIN)
                .chain_update(endpoint.as_bytes())
                .finalize()
                .into(),
        )
        .ok_or_else(|| anyhow!("zero relay identity"))?;
        Ok(Self {
            endpoint,
            relay,
            share,
        })
    }

    #[must_use]
    pub fn object_name(&self) -> String {
        URL_SAFE_NO_PAD.encode(self.share.as_bytes())
    }
}

pub fn limits() -> Limits {
    let capacity =
        |frames| Counts::new(frames, frames * u64::from(OpaqueHeader::MAX_ENCODED_BYTES));
    Limits {
        business_connections: MAX_BUSINESS_CONNECTIONS,
        handshakes: windshare_relay_core::share::HandshakeLimits {
            concurrent: CONTROL_CONNECTIONS,
            attempts: CONNECTION_ATTEMPTS,
        },
        sessions: RETAINED_FLOWS,
        retained_flows_per_connection: RETAINED_FLOWS,
        // This is an admission-risk policy, never a receipt for hidden buffers.
        retirement: RetirementPolicy::Bounded {
            admission_grace: Duration::ZERO,
            risk_retention: LOST_GENERATION_RISK_RETENTION,
            generations: MAX_BUSINESS_CONNECTIONS,
            residual: capacity(CONNECTION_FRAMES * MAX_BUSINESS_CONNECTIONS as u64),
        },
        delivery: flow::Limits::new(
            capacity(CONNECTION_FRAMES),
            capacity(2),
            capacity(1),
            SETTLEMENT_TIMEOUT,
        )
        .expect("positive Cloudflare capacity"),
    }
}

pub const fn budget() -> CheckpointBudget {
    CheckpointBudget::new(ATTACHMENT_BYTES, ATTACHMENT_ENVELOPE_BYTES)
        .expect("bounded binary attachment")
}

/// Keep an earlier alarm: moving a deadline later must not cause a storage write.
pub fn earlier_alarm(scheduled: Option<u64>, deadline: Option<u64>) -> Option<u64> {
    deadline.filter(|at| scheduled.is_none_or(|existing| *at < existing))
}

#[cfg(test)]
#[path = "config_tests.rs"]
mod tests;
