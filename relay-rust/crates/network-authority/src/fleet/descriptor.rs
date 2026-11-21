use super::{
    signature::verify_bytes,
    topology::{Role, public_key, public_origin, stun_host},
};
use crate::{Fault, Zone};
use anyhow::{Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD as B64};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

pub const MAX_DESCRIPTOR: usize = 16 * 1024;
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
struct Descriptor {
    version: u8,
    node_id: String,
    node_key: String,
    relay_endpoint: String,
    services: Services,
    stun: Vec<String>,
    zone: Zone,
    region: String,
    capacity_bytes_per_second: Option<u64>,
    issued_at: u64,
    expires_at: u64,
}
#[derive(Serialize, Deserialize)]
pub struct Discovery {
    pub node_key: String,
    pub relay: String,
    pub roles: Vec<Role>,
    pub stun: Vec<String>,
    pub zone: Zone,
    pub region: String,
    pub capacity_mbps: Option<u32>,
}

/// # Errors
/// Rejects invalid or stale signatures, mismatched endpoint identities and unusable service settings.
pub fn inspect(origin: &str, bytes: &[u8], now: u64) -> Result<Discovery> {
    let origin = public_origin(origin)?;
    ensure!(bytes.len() <= MAX_DESCRIPTOR, Fault::Invalid);
    let envelope: Envelope = serde_json::from_slice(bytes)?;
    ensure!(envelope.version == 2, Fault::Invalid);
    let payload = B64.decode(envelope.descriptor)?;
    let value: Descriptor = serde_json::from_slice(&payload)?;
    verify_bytes(
        &value.node_key,
        b"windshare/node-descriptor/v2\0",
        &payload,
        &envelope.signature,
    )?;
    let mut digest = Sha256::new();
    digest.update(b"windshare/node-identity/v1\0");
    digest.update(public_key(&value.node_key)?);
    let host = origin.strip_prefix("https://").ok_or(Fault::Invalid)?;
    ensure!(
        value.version == 2
            && value.node_id == B64.encode(digest.finalize())
            && value.relay_endpoint == format!("wss://{host}/v2/ws")
            && value.expires_at > now
            && value.issued_at <= now + 60
            && value.expires_at > value.issued_at
            && value.expires_at - value.issued_at <= 300,
        Fault::Invalid
    );
    let mut roles = Vec::new();
    if value.services.relay {
        roles.push(Role::Relay);
    }
    if value.services.stun {
        roles.push(Role::Stun);
    }
    ensure!(
        !roles.is_empty()
            && value.stun.len() <= 4
            && (!value.services.stun || !value.stun.is_empty()),
        Fault::Invalid
    );
    let mut stun = Vec::new();
    if value.services.stun {
        let url = url::Url::parse(&origin)?;
        for address in value.stun {
            ensure!(
                Some(stun_host(&address)?.as_str()) == url.host_str() && !stun.contains(&address),
                Fault::Invalid
            );
            stun.push(address);
        }
    }
    let capacity_mbps = value
        .capacity_bytes_per_second
        .and_then(|n| u32::try_from(n.div_ceil(125_000)).ok());
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
