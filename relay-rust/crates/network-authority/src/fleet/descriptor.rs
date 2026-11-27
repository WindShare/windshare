use super::{
    signature::verify_bytes,
    topology::{Role, public_key, public_origin, stun_host},
};
use crate::{DiscoveryFailure, Zone};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD as B64};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::fmt;

pub const MAX_DESCRIPTOR: usize = 16 * 1024;
const MAX_CLOCK_SKEW_SECONDS: u64 = 60;
const MAX_DESCRIPTOR_LIFETIME_SECONDS: u64 =
    windshare_relay_access::descriptor::Purpose::Live.lifetime();
use windshare_relay_access::descriptor::{
    DESCRIPTOR_VERSION, InvariantError, NODE_IDENTITY_DOMAIN, NODE_PLATFORM_NATIVE,
    validate_service_invariants,
};
const BYTES_PER_MBPS: u64 = 125_000;

#[derive(Debug, PartialEq, Eq)]
pub struct DescriptorError {
    pub failure: DiscoveryFailure,
    pub detail: String,
}
impl DescriptorError {
    fn new(failure: DiscoveryFailure, detail: impl Into<String>) -> Self {
        Self {
            failure,
            detail: detail.into(),
        }
    }

    fn invalid(detail: impl Into<String>) -> Self {
        Self::new(DiscoveryFailure::InvalidDescriptor, detail)
    }

    fn expired(detail: impl Into<String>) -> Self {
        Self::new(DiscoveryFailure::DescriptorExpired, detail)
    }

    fn mismatch(detail: impl Into<String>) -> Self {
        Self::new(DiscoveryFailure::DescriptorMismatch, detail)
    }
}
impl fmt::Display for DescriptorError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&self.detail)
    }
}
impl std::error::Error for DescriptorError {}

#[derive(Deserialize)]
struct Envelope {
    version: u8,
    descriptor: String,
    signature: String,
}
#[derive(Deserialize)]
struct Services {
    relay: bool,
    stun: bool,
}
#[derive(Deserialize)]
struct Authority {
    #[serde(default)]
    url: String,
    #[serde(default)]
    issuer: String,
}
#[derive(Deserialize)]
struct Descriptor {
    authority: Option<Authority>,
    platform: String,
    version: u8,
    node_id: String,
    node_key: String,
    relay_endpoint: String,
    services: Services,
    public_relay: windshare_relay_access::descriptor::PublicRelay,
    stun: Vec<String>,
    zone: Zone,
    region: String,
    public_capacity_bytes_per_second: Option<u64>,
    issued_at: u64,
    expires_at: u64,
}
#[derive(Debug, Serialize, Deserialize)]
pub struct Discovery {
    pub node_key: String,
    pub relay: String,
    pub roles: Vec<Role>,
    pub stun: Vec<String>,
    pub zone: Zone,
    pub region: String,
    pub capacity_mbps: Option<u32>,
}

fn decode_verified(bytes: &[u8]) -> Result<(Descriptor, [u8; 32]), DescriptorError> {
    if bytes.len() > MAX_DESCRIPTOR {
        return Err(DescriptorError::invalid(format!(
            "descriptor exceeds {MAX_DESCRIPTOR} bytes"
        )));
    }
    let envelope: Envelope = serde_json::from_slice(bytes)
        .map_err(|error| DescriptorError::invalid(format!("malformed descriptor JSON: {error}")))?;
    if envelope.version != DESCRIPTOR_VERSION {
        return Err(DescriptorError::invalid(format!(
            "unsupported descriptor envelope version {}",
            envelope.version
        )));
    }
    let payload = B64.decode(envelope.descriptor).map_err(|error| {
        DescriptorError::invalid(format!("invalid descriptor base64 payload: {error}"))
    })?;
    let value: Descriptor = serde_json::from_slice(&payload).map_err(|error| {
        DescriptorError::invalid(format!("malformed descriptor payload JSON: {error}"))
    })?;
    verify_bytes(
        &value.node_key,
        windshare_relay_access::descriptor::Purpose::Live.domain(),
        &payload,
        &envelope.signature,
    )
    .map_err(|error| {
        DescriptorError::invalid(format!("descriptor signature verification failed: {error}"))
    })?;
    if value.version != DESCRIPTOR_VERSION {
        return Err(DescriptorError::invalid(format!(
            "unsupported descriptor payload version {}",
            value.version
        )));
    }
    if value.platform != NODE_PLATFORM_NATIVE {
        return Err(DescriptorError::invalid("unsupported node platform"));
    }
    let key = public_key(&value.node_key)
        .map_err(|error| DescriptorError::invalid(format!("invalid node_key: {error}")))?;
    Ok((value, key))
}

fn validate_identity_and_endpoint(
    origin: &str,
    value: &Descriptor,
    key: &[u8; 32],
) -> Result<(), DescriptorError> {
    let mut digest = Sha256::new();
    digest.update(NODE_IDENTITY_DOMAIN);
    digest.update(key);
    let expected_node_id = B64.encode(digest.finalize());
    if value.node_id != expected_node_id {
        return Err(DescriptorError::mismatch(
            "descriptor node_id does not match node_key",
        ));
    }
    let host = origin
        .strip_prefix("https://")
        .ok_or_else(|| DescriptorError::new(DiscoveryFailure::InvalidUrl, "non-HTTPS origin"))?;
    let expected_endpoint = format!("wss://{host}/v2/ws");
    if value.relay_endpoint != expected_endpoint {
        return Err(DescriptorError::mismatch(format!(
            "descriptor relay_endpoint '{}' does not match expected '{expected_endpoint}'",
            value.relay_endpoint
        )));
    }
    Ok(())
}

fn validate_freshness(value: &Descriptor, now: u64) -> Result<(), DescriptorError> {
    if value.expires_at <= now {
        return Err(DescriptorError::expired(format!(
            "descriptor expired at {} (authority time {now})",
            value.expires_at
        )));
    }
    if value.issued_at > now.saturating_add(MAX_CLOCK_SKEW_SECONDS) {
        return Err(DescriptorError::expired(format!(
            "descriptor issued_at {} exceeds authority clock skew (authority time {now})",
            value.issued_at
        )));
    }
    if value.expires_at <= value.issued_at
        || value.expires_at - value.issued_at > MAX_DESCRIPTOR_LIFETIME_SECONDS
    {
        return Err(DescriptorError::expired(format!(
            "descriptor lifetime {}..{} is out of bounds",
            value.issued_at, value.expires_at
        )));
    }
    Ok(())
}

fn validate_services(
    origin: &str,
    value: &Descriptor,
) -> Result<(Vec<Role>, Vec<String>), DescriptorError> {
    validate_service_invariants(
        value.services.relay,
        value.services.stun,
        value.stun.len(),
        value.public_relay,
    )
    .map_err(|error| match error {
        InvariantError::EnabledWithoutRelayService => {
            DescriptorError::mismatch("enabled public relay requires the relay service")
        }
        InvariantError::TooManyStunUrls | InvariantError::StunServiceWithoutUrls => {
            DescriptorError::mismatch("descriptor STUN URL count does not match declared services")
        }
    })?;
    let mut roles = Vec::new();
    if value.services.relay {
        roles.push(Role::Relay);
    }
    if value.services.stun {
        roles.push(Role::Stun);
    }
    if roles.is_empty() {
        return Err(DescriptorError::mismatch(
            "descriptor advertises neither relay nor STUN service",
        ));
    }
    let mut stun = Vec::new();
    if value.services.stun {
        let url = url::Url::parse(origin).map_err(|error| {
            DescriptorError::new(DiscoveryFailure::InvalidUrl, error.to_string())
        })?;
        for address in &value.stun {
            let host = stun_host(address).map_err(|_| {
                DescriptorError::mismatch(format!("malformed descriptor STUN URL '{address}'"))
            })?;
            if Some(host.as_str()) != url.host_str() {
                return Err(DescriptorError::mismatch(format!(
                    "descriptor STUN host '{host}' does not match origin host"
                )));
            }
            if stun.contains(address) {
                return Err(DescriptorError::mismatch(format!(
                    "descriptor contains duplicate STUN URL '{address}'"
                )));
            }
            stun.push(address.clone());
        }
    }
    Ok((roles, stun))
}

fn authority_origin(raw: &str) -> Option<String> {
    if raw.trim_matches(|c: char| c.is_whitespace() || c == '\u{feff}') != raw
        || raw.bytes().any(|byte| byte.is_ascii_control())
        || raw.contains(['?', '#', '\\', '@'])
    {
        return None;
    }
    let url = url::Url::parse(raw).ok()?;
    if !matches!(url.scheme(), "https" | "http")
        || url.host().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || url.port() == Some(0)
        || url.path() != "/"
    {
        return None;
    }
    // Reject dot segments and extra slashes that `url::Url::parse` folds to `/`.
    let after_scheme = raw.split_once("://")?.1;
    if after_scheme
        .split_once('/')
        .is_some_and(|(_, path)| !path.is_empty())
    {
        return None;
    }
    Some(url.origin().ascii_serialization())
}

fn matches_authority(
    authority: Option<&Authority>,
    issuer: &str,
    authority_origins: &[&str],
) -> bool {
    let Some(authority) = authority else {
        return false;
    };
    if authority.issuer != issuer {
        return false;
    }
    let Some(actual) = authority_origin(&authority.url) else {
        return false;
    };
    authority_origins
        .iter()
        .filter_map(|origin| authority_origin(origin))
        .any(|expected| expected == actual)
}

/// # Errors
/// Rejects invalid or stale signatures, mismatched endpoint identities and unusable service settings.
pub fn inspect(
    origin: &str,
    bytes: &[u8],
    now: u64,
    issuer: &str,
    authority_origins: &[&str],
) -> Result<Discovery, DescriptorError> {
    let origin = public_origin(origin)
        .map_err(|_| DescriptorError::new(DiscoveryFailure::InvalidUrl, "invalid public origin"))?;
    let (value, key) = decode_verified(bytes)?;
    validate_identity_and_endpoint(&origin, &value, &key)?;
    validate_freshness(&value, now)?;
    if !matches_authority(value.authority.as_ref(), issuer, authority_origins) {
        return Err(DescriptorError::new(
            DiscoveryFailure::AuthorityMismatch,
            "node authority issuer or URL does not match this authority",
        ));
    }
    let (roles, stun) = validate_services(&origin, &value)?;
    let capacity_mbps = value
        .public_capacity_bytes_per_second
        .and_then(|n| u32::try_from(n.div_ceil(BYTES_PER_MBPS)).ok());
    Ok(Discovery {
        node_key: value.node_key,
        relay: origin,
        roles,
        stun,
        zone: value.zone,
        region: value.region,
        capacity_mbps,
    })
}

#[cfg(test)]
#[path = "descriptor_tests.rs"]
mod tests;
