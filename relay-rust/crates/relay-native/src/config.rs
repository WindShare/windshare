use anyhow::{Result, bail, ensure};
use sha2::{Digest, Sha256};
use std::time::Duration;
use windshare_relay_core::{flow, identifiers::RelayIdentity, share};

pub const MAILBOX_CAPACITY: usize = 32;
pub const CONTROL_QUEUE_FRAMES: usize = 8;
pub const DATA_QUEUE_FRAMES: usize = 64;
pub const DATA_QUEUE_BYTES: usize = DATA_QUEUE_FRAMES * MAX_DATA_BYTES;
pub const SUBMITTED_DATA_FRAMES: usize = 2;
pub const SUBMITTED_DATA_BYTES: usize = 2 * MAX_DATA_BYTES;
pub const DELIVERY_WINDOW: flow::Counts = flow::Counts::new(16, 16 * MAX_DATA_BYTES as u64);
const MAX_DATA_BYTES: usize =
    windshare_relay_core::sessions::OpaqueHeader::MAX_ENCODED_BYTES as usize;
pub const MAX_BUSINESS_CONNECTIONS: usize = 256;
pub const MAX_CONTROL_CONNECTIONS: usize = 32;
pub const MAX_HTTP_REQUESTS: usize = 64;
pub const HTTP_REQUEST_TIMEOUT: Duration = Duration::from_secs(5);
pub const CONNECTION_ATTEMPTS: share::AttemptRate = share::AttemptRate {
    burst: 512,
    refill: Duration::from_millis(10),
};
pub const MAX_RESIDENT_SHARES: usize = 1024;
const SHARE_BUSINESS_CONNECTIONS: usize = 32;
const SHARE_CONTROL_CONNECTIONS: usize = 8;
const SHARE_CONNECTION_ATTEMPTS: share::AttemptRate = share::AttemptRate {
    burst: 64,
    refill: Duration::from_millis(100),
};
pub const SOCKET_TIMEOUT: Duration = Duration::from_secs(5);
pub const STORAGE_RETRY: Duration = Duration::from_millis(250);
pub const MAX_CONTROL_BYTES: usize = windshare_relay_core::descriptor_upload::MAX_DESCRIPTOR_BYTES
    + windshare_relay_core::sessions::DESCRIPTOR_HEADER_BYTES;
pub const MAX_MESSAGE_BYTES: usize = if MAX_DATA_BYTES > MAX_CONTROL_BYTES {
    MAX_DATA_BYTES
} else {
    MAX_CONTROL_BYTES
};
const RELAY_IDENTITY_DOMAIN: &[u8] = b"windshare/v2 relay-identity\0";
const WEBSOCKET_PATH: &str = "/v2/ws";

/// An explicit canonical public WebSocket endpoint, independent of the bind address.
#[derive(Clone, Debug)]
pub struct Endpoint {
    pub(crate) url: String,
    pub(crate) path: String,
    pub(crate) identity: RelayIdentity,
}

impl Endpoint {
    /// # Errors
    /// Rejects noncanonical URLs instead of silently signing different endpoint bytes.
    pub fn parse(raw: &str) -> Result<Self> {
        let url = url::Url::parse(raw)?;
        ensure!(
            raw.is_ascii()
                && raw == url.as_str()
                && matches!(url.scheme(), "ws" | "wss")
                && url.username().is_empty()
                && url.password().is_none()
                && url.query().is_none()
                && url.fragment().is_none()
                && url.port() != Some(0)
                && url.path().ends_with(WEBSOCKET_PATH),
            "expected a canonical ws(s)://host[/base]/v2/ws endpoint without credentials or query"
        );
        let host = url
            .host_str()
            .ok_or_else(|| anyhow::anyhow!("missing host"))?;
        if !host.starts_with('[') && host.parse::<std::net::Ipv4Addr>().is_err() {
            validate_dns(host)?;
        }
        let digest = Sha256::new()
            .chain_update(RELAY_IDENTITY_DOMAIN)
            .chain_update(raw.as_bytes())
            .finalize()
            .into();
        Ok(Self {
            url: raw.to_owned(),
            path: url.path().to_owned(),
            identity: RelayIdentity::new(digest).ok_or_else(|| anyhow::anyhow!("zero identity"))?,
        })
    }

    #[must_use]
    pub fn as_str(&self) -> &str {
        &self.url
    }
}

fn validate_dns(host: &str) -> Result<()> {
    let name = host.strip_suffix('.').unwrap_or(host);
    ensure!(!name.is_empty() && name.len() <= 253, "invalid host");
    for label in name.split('.') {
        if label.is_empty()
            || label.len() > 63
            || label.starts_with('-')
            || label.ends_with('-')
            || label.starts_with("xn--")
            || label.as_bytes().get(2..4) == Some(b"--")
            || !label
                .bytes()
                .all(|b| b.is_ascii_alphanumeric() || b == b'-')
        {
            bail!("host does not match the relay identity contract");
        }
    }
    Ok(())
}

pub fn limits() -> share::Limits {
    // Native budgets are independent of Worker attachment and scheduling limits.
    let frame_bytes = u64::from(windshare_relay_core::sessions::OpaqueHeader::MAX_ENCODED_BYTES);
    let capacity = |frames| flow::Counts::new(frames, frames * frame_bytes);
    share::Limits {
        business_connections: SHARE_BUSINESS_CONNECTIONS,
        handshakes: share::HandshakeLimits {
            concurrent: SHARE_CONTROL_CONNECTIONS,
            attempts: SHARE_CONNECTION_ATTEMPTS,
        },
        sessions: 64,
        retained_flows_per_connection: 64,
        retirement: share::RetirementPolicy::PhysicalRelease,
        delivery: flow::Limits::new(
            flow::Counts::new(DATA_QUEUE_FRAMES as u64, DATA_QUEUE_BYTES as u64),
            capacity(2),
            capacity(1),
            Duration::from_secs(5),
        )
        .expect("positive native capacity"),
    }
}

#[cfg(test)]
#[path = "config_tests.rs"]
mod tests;
