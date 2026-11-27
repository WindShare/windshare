use super::transport;
use anyhow::{Context, Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD as B64};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use url::Url;
use windshare_relay_access::{
    descriptor::{PublicRelay, Purpose},
    trust,
};

pub(super) const MAX_RECORD: usize = 16 * 1024;
const CLOCK_SKEW: u64 = 60;

#[derive(Clone, Deserialize, Serialize)]
pub(super) struct Envelope {
    version: u8,
    descriptor: String,
    signature: String,
}

#[derive(Deserialize)]
struct Payload {
    version: u8,
    platform: String,
    node_key: String,
    node_id: String,
    relay_endpoint: String,
    public_relay: PublicRelay,
    issued_at: u64,
    expires_at: u64,
}

pub struct Record {
    pub(super) envelope: Envelope,
    pub(super) key: String,
    pub(super) endpoint: String,
    pub(super) origin: Url,
    pub(super) expires: u64,
    pub(super) issued: u64,
    pub(super) public_relay: PublicRelay,
}

impl Envelope {
    pub fn verify(self, purpose: Purpose, now: u64) -> Result<Record> {
        ensure!(
            self.version == 2 && self.descriptor.len() + self.signature.len() < MAX_RECORD,
            "invalid envelope"
        );
        let mut bytes = B64.decode(&self.descriptor)?;
        let value: Payload = serde_json::from_slice(&bytes)?;
        let key: [u8; 32] = B64
            .decode(&value.node_key)?
            .try_into()
            .ok()
            .context("node key width")?;
        bytes.extend_from_slice(&B64.decode(&self.signature)?);
        let verified = trust::verify(purpose.domain(), &bytes, &key)
            .map_err(|_| anyhow::anyhow!("invalid signature"))?;
        ensure!(
            verified.len() + 64 == bytes.len() && self.signature.len() == 86,
            "invalid signature width"
        );
        ensure!(
            value.version == 2
                && value.platform == "native"
                && value.node_id
                    == B64.encode(
                        Sha256::new()
                            .chain_update(b"windshare/node-identity/v1\0")
                            .chain_update(key)
                            .finalize()
                    ),
            "invalid identity"
        );
        ensure!(
            value.issued_at <= now.saturating_add(CLOCK_SKEW)
                && now < value.expires_at
                && (1..=purpose.lifetime())
                    .contains(&value.expires_at.saturating_sub(value.issued_at)),
            "descriptor lifetime"
        );
        let endpoint = crate::Endpoint::parse(&value.relay_endpoint)?;
        let origin = transport::origin(endpoint.relay_url())?;
        Ok(Record {
            envelope: self,
            key: value.node_key,
            endpoint: value.relay_endpoint,
            origin,
            expires: value.expires_at,
            issued: value.issued_at,
            public_relay: value.public_relay,
        })
    }
}
