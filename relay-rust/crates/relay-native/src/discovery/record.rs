use super::transport;
use anyhow::{Context, Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD as B64};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use url::Url;
use windshare_relay_access::{
    descriptor::{
        DESCRIPTOR_VERSION, InvariantError, NODE_IDENTITY_DOMAIN, NODE_PLATFORM_NATIVE,
        PublicRelay, Purpose, offers_community_service, validate_service_invariants,
    },
    trust,
};

pub(super) const MAX_RECORD: usize = 16 * 1024;
const CLOCK_SKEW: u64 = 60;
const SIGNATURE_B64_LEN: usize = 86;

#[derive(Clone, Deserialize, Serialize)]
pub(super) struct Envelope {
    version: u8,
    descriptor: String,
    signature: String,
}

impl From<crate::node::GossipEnvelope> for Envelope {
    fn from(envelope: crate::node::GossipEnvelope) -> Self {
        Self {
            version: envelope.version,
            descriptor: envelope.descriptor,
            signature: envelope.signature,
        }
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq)]
pub(super) struct Services {
    pub(super) relay: bool,
    pub(super) stun: bool,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq)]
pub(super) struct Authority {
    pub(super) url: String,
    pub(super) issuer: String,
}

#[derive(Deserialize)]
struct Payload {
    version: u8,
    platform: String,
    node_key: String,
    node_id: String,
    relay_endpoint: String,
    services: Services,
    stun: Vec<String>,
    #[serde(default)]
    authority: Option<Authority>,
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
    pub(super) services: Services,
    pub(super) authority: Option<Authority>,
    pub(super) public_relay: PublicRelay,
}

impl Record {
    pub(super) const fn offers_community_service(&self) -> bool {
        offers_community_service(
            self.authority.is_some(),
            self.services.relay,
            self.services.stun,
            self.public_relay,
        )
    }
}

fn decode_public_key(raw: &str) -> Result<[u8; 32]> {
    let key: [u8; 32] = B64
        .decode(raw)?
        .try_into()
        .ok()
        .context("public key width")?;
    ensure!(
        key != [0; 32] && B64.encode(key) == raw,
        "invalid public key"
    );
    Ok(key)
}

impl Envelope {
    pub fn verify(self, purpose: Purpose, now: u64) -> Result<Record> {
        ensure!(
            self.version == DESCRIPTOR_VERSION
                && self.descriptor.len() + self.signature.len() < MAX_RECORD,
            "invalid envelope"
        );
        let mut bytes = B64.decode(&self.descriptor)?;
        let value: Payload = serde_json::from_slice(&bytes)?;
        let key = decode_public_key(&value.node_key).context("node key")?;
        bytes.extend_from_slice(&B64.decode(&self.signature)?);
        let verified = trust::verify(purpose.domain(), &bytes, &key)
            .map_err(|_| anyhow::anyhow!("invalid signature"))?;
        ensure!(
            verified.len() + 64 == bytes.len() && self.signature.len() == SIGNATURE_B64_LEN,
            "invalid signature width"
        );
        ensure!(
            value.version == DESCRIPTOR_VERSION
                && value.platform == NODE_PLATFORM_NATIVE
                && value.node_id
                    == B64.encode(
                        Sha256::new()
                            .chain_update(NODE_IDENTITY_DOMAIN)
                            .chain_update(key)
                            .finalize()
                    ),
            "invalid identity"
        );
        if let Some(authority) = &value.authority {
            decode_public_key(&authority.issuer).context("authority issuer")?;
        }
        validate_service_invariants(
            value.services.relay,
            value.services.stun,
            value.stun.len(),
            value.public_relay,
        )
        .map_err(|error| match error {
            InvariantError::EnabledWithoutRelayService => {
                anyhow::anyhow!("enabled public relay requires relay service")
            }
            InvariantError::TooManyStunUrls => anyhow::anyhow!("too many STUN URLs"),
            InvariantError::StunServiceWithoutUrls => {
                anyhow::anyhow!("STUN service requires at least one STUN URL")
            }
        })?;
        ensure!(
            value.issued_at <= now.saturating_add(CLOCK_SKEW)
                && now < value.expires_at
                && (1..=purpose.lifetime())
                    .contains(&value.expires_at.saturating_sub(value.issued_at)),
            "descriptor lifetime"
        );
        let endpoint = crate::Endpoint::parse(&value.relay_endpoint)?;
        let origin = transport::relay_origin(endpoint.relay_url())?;
        Ok(Record {
            envelope: self,
            key: value.node_key,
            endpoint: value.relay_endpoint,
            origin,
            expires: value.expires_at,
            issued: value.issued_at,
            services: value.services,
            authority: value.authority,
            public_relay: value.public_relay,
        })
    }
}
