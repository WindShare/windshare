use anyhow::{Result, bail, ensure};
use sha2::{Digest, Sha256};
use std::{net::IpAddr, time::Duration};
use windshare_relay_core::{flow, identifiers::RelayIdentity, share};

pub const MAILBOX_CAPACITY: usize = 32;
pub const CONTROL_QUEUE_FRAMES: usize = 8;
// Keep the existing advisory burst without using protocol control slots.
pub const CAPACITY_NOTICE_QUEUE_FRAMES: usize = 8;
// Connection-wide delivery bounds. Receivers' ToSender envelopes carry small
// signaling and block-request messages rather than file payloads, so a
// connection may hold more frames than full-sized frame bytes while bytes still
// bound memory. 256 frames matches Go's default receive capacity declaration.
pub const CONNECTION_DATA_FRAMES: usize = 256;
pub const CONNECTION_DATA_BYTES: usize = 64 * MAX_DATA_BYTES;
// Per-flow pending frame cap and simultaneous queued-flow cap per connection.
pub const FLOW_PENDING_FRAMES: usize = 64;
pub const CONNECTION_PENDING_FLOWS: usize = 64;
pub const SUBMITTED_DATA_FRAMES: usize = 2;
pub const SUBMITTED_DATA_BYTES: usize = 2 * MAX_DATA_BYTES;
// ToReceiver carries bulk file fragments on each receiver's dedicated ledger.
pub const TO_RECEIVER_DELIVERY_WINDOW: flow::Counts =
    flow::Counts::new(16, 16 * MAX_DATA_BYTES as u64);
// Every receiver's ToSender flow shares the sender connection's ledger and
// carries only small control/request envelopes. Sizing each flow to 8 frames
// (matching the receiver block broker's default concurrency) and 1 full frame
// of bytes lets all 31 business receivers hold their full window concurrently
// without join-order starvation. At full occupancy (31 * 8 = 248 frames) half
// the sender byte pool stays free while the 256-frame connection bound leaves
// 6 ordinary plus 2 startup frames for light settling churn; heavier churn
// waits for WS2S or the 5s settlement timeout.
pub const TO_SENDER_DELIVERY_WINDOW: flow::Counts = flow::Counts::new(8, MAX_DATA_BYTES as u64);
const STARTUP_RESERVE_FRAMES: u64 = 2;
const SHARE_MAX_RECEIVERS: u64 = SHARE_BUSINESS_CONNECTIONS as u64 - 1;
pub const SHARE_MAX_SESSIONS: usize = 64;
const CONNECTION_RETAINED_FLOWS: usize = 64;
const _: () = assert!(
    TO_SENDER_DELIVERY_WINDOW.frames > 0
        && TO_SENDER_DELIVERY_WINDOW.encoded_bytes >= MAX_DATA_BYTES as u64
        && TO_RECEIVER_DELIVERY_WINDOW.frames > 0
        && TO_RECEIVER_DELIVERY_WINDOW.encoded_bytes >= MAX_DATA_BYTES as u64
        && TO_SENDER_DELIVERY_WINDOW.frames * SHARE_MAX_RECEIVERS
            <= CONNECTION_DATA_FRAMES as u64 - STARTUP_RESERVE_FRAMES
        && TO_SENDER_DELIVERY_WINDOW.encoded_bytes * SHARE_MAX_RECEIVERS
            <= CONNECTION_DATA_BYTES as u64 - STARTUP_RESERVE_FRAMES * MAX_DATA_BYTES as u64
        && TO_SENDER_DELIVERY_WINDOW.frames <= FLOW_PENDING_FRAMES as u64
        && TO_RECEIVER_DELIVERY_WINDOW.frames <= FLOW_PENDING_FRAMES as u64
        && SHARE_MAX_RECEIVERS <= SHARE_MAX_SESSIONS as u64
        && SHARE_MAX_RECEIVERS <= CONNECTION_RETAINED_FLOWS as u64
        && CONNECTION_RETAINED_FLOWS <= CONNECTION_PENDING_FLOWS
);
const MAX_DATA_BYTES: usize =
    windshare_relay_core::sessions::OpaqueHeader::MAX_ENCODED_BYTES as usize;
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
    relay_url: String,
    pub(crate) path: String,
    pub(crate) identity: RelayIdentity,
}

impl Endpoint {
    /// # Errors
    /// Rejects unspecified hosts and noncanonical URLs instead of silently signing
    /// different endpoint bytes.
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
                && url.port() != Some(0),
            "expected a canonical ws(s)://host[/base]/v2/ws endpoint without credentials or query"
        );
        let mount = url.path().strip_suffix(WEBSOCKET_PATH).ok_or_else(|| {
            anyhow::anyhow!("expected a canonical ws(s)://host[/base]/v2/ws endpoint")
        })?;
        let host = url.host().ok_or_else(|| anyhow::anyhow!("missing host"))?;
        match host {
            url::Host::Domain(name) => validate_dns(name)?,
            url::Host::Ipv4(address) => validate_endpoint_address(address.into())?,
            url::Host::Ipv6(address) => validate_endpoint_address(address.into())?,
        }
        let digest = Sha256::new()
            .chain_update(RELAY_IDENTITY_DOMAIN)
            .chain_update(raw.as_bytes())
            .finalize()
            .into();
        let scheme = if url.scheme() == "wss" {
            "https"
        } else {
            "http"
        };
        let authority = &url[url::Position::BeforeHost..url::Position::AfterPort];
        let mut relay_url = format!("{scheme}://{authority}{mount}");
        // Clients trim one optional trailing slash before appending the route.
        // Preserve an empty path segment that belongs to the mount itself.
        if mount.ends_with('/') {
            relay_url.push('/');
        }
        Ok(Self {
            url: raw.to_owned(),
            relay_url,
            path: url.path().to_owned(),
            identity: RelayIdentity::new(digest).ok_or_else(|| anyhow::anyhow!("zero identity"))?,
        })
    }

    pub fn relay_url(&self) -> &str {
        &self.relay_url
    }

    pub fn as_str(&self) -> &str {
        &self.url
    }
}

fn validate_endpoint_address(address: IpAddr) -> Result<()> {
    // Wildcards select listening interfaces; they cannot identify a remote relay.
    ensure!(
        !address.to_canonical().is_unspecified(),
        "public endpoint host {address} is unspecified; use the address or DNS name clients connect to"
    );
    Ok(())
}

fn validate_dns(host: &str) -> Result<()> {
    let name = host.strip_suffix('.').unwrap_or(host);
    ensure!(!name.is_empty() && name.len() <= 253, "invalid host");
    for label in name.split('.') {
        if label.is_empty()
            || label.len() > 63
            || label.starts_with('-')
            || label.ends_with('-')
            || (label.as_bytes().get(2..4) == Some(b"--") && !label.starts_with("xn--"))
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
        sessions: SHARE_MAX_SESSIONS,
        retained_flows_per_connection: CONNECTION_RETAINED_FLOWS,
        retirement: share::RetirementPolicy::PhysicalRelease,
        delivery: flow::Limits::new(
            flow::Counts::new(CONNECTION_DATA_FRAMES as u64, CONNECTION_DATA_BYTES as u64),
            capacity(STARTUP_RESERVE_FRAMES),
            capacity(1),
            Duration::from_secs(5),
        )
        .expect("positive native capacity"),
    }
}

#[cfg(test)]
#[path = "config_tests.rs"]
mod tests;
