use super::topology::{
    Entry, MAX_COUNTER, MAX_NODES, PublicStun, null_list, public_key, stun_host, valid_text,
};
use anyhow::{Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD as B64};
use chrono::{DateTime, Duration, Utc};
use ed25519_dalek::{Signature, Signer, SigningKey, VerifyingKey};
use serde::{Deserialize, Serialize};
use std::collections::BTreeSet;

pub const MAX_BYTES: usize = 512 * 1024;
const CERT_DOMAIN: &[u8] = b"windshare/network-certificate/v1\0";
const PAYLOAD_DOMAIN: &[u8] = b"windshare/network-directory/v1\0";
pub const RENEW_BEFORE: Duration = Duration::days(7);
const PAYLOAD_LIFETIME: Duration = Duration::days(90);

#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct Certificate {
    pub root: String,
    pub payload: String,
    pub signature: String,
}
#[derive(Clone, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
pub struct Envelope {
    pub payload: String,
    pub certificate: Certificate,
    pub signature: String,
}
#[derive(Deserialize)]
pub struct Root {
    pub id: String,
    pub key: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Grant {
    format: u8,
    network: String,
    pub generation: u64,
    pub key: String,
    pub not_before: DateTime<Utc>,
    pub expires_at: DateTime<Utc>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Payload {
    format: u8,
    network: String,
    pub generation: u64,
    pub serial: u64,
    pub issued_at: DateTime<Utc>,
    pub expires_at: DateTime<Utc>,
    #[serde(default, deserialize_with = "null_list")]
    pub nodes: Vec<Entry>,
    #[serde(default, deserialize_with = "null_list")]
    pub public_stun: Vec<PublicStun>,
}

/// # Errors
/// Rejects malformed keys or signatures that do not cover the exact domain and bytes.
pub fn verify_bytes(key: &str, domain: &[u8], bytes: &[u8], signature: &str) -> Result<()> {
    let key = VerifyingKey::from_bytes(&public_key(key)?)?;
    let signature = Signature::from_slice(&B64.decode(signature)?)?;
    Ok(key.verify_strict(&[domain, bytes].concat(), &signature)?)
}

impl Certificate {
    /// # Errors
    /// Rejects untrusted roots, invalid signatures and invalid signing grants.
    pub fn verify(&self, network: &str, roots: &[Root]) -> Result<Grant> {
        let mut matches = roots.iter().filter(|r| r.id == self.root);
        let root = matches
            .next()
            .ok_or_else(|| anyhow::anyhow!("unknown directory root"))?;
        ensure!(
            matches.next().is_none() && self.payload.len() <= 5462,
            "invalid directory certificate"
        );
        let bytes = B64.decode(&self.payload)?;
        verify_bytes(&root.key, CERT_DOMAIN, &bytes, &self.signature)?;
        let grant: Grant = serde_json::from_slice(&bytes)?;
        ensure!(
            grant.format == 1
                && grant.network == network
                && (1..=MAX_COUNTER).contains(&grant.generation)
                && grant.expires_at > grant.not_before
                && grant.expires_at - grant.not_before <= Duration::days(180),
            "invalid directory grant"
        );
        public_key(&grant.key)?;
        Ok(grant)
    }
}
impl Envelope {
    /// # Errors
    /// Rejects invalid signatures, directory bounds and topology inconsistent with the grant.
    pub fn verify(&self, network: &str, roots: &[Root], history: bool) -> Result<Payload> {
        ensure!(self.payload.len() < MAX_BYTES, "directory too large");
        let grant = self.certificate.verify(network, roots)?;
        let bytes = B64.decode(&self.payload)?;
        verify_bytes(&grant.key, PAYLOAD_DOMAIN, &bytes, &self.signature)?;
        let payload: Payload = serde_json::from_slice(&bytes)?;
        ensure!(
            payload.format == 1
                && payload.network == network
                && payload.generation == grant.generation
                && payload.issued_at >= grant.not_before
                && payload.expires_at > payload.issued_at
                && payload.expires_at - payload.issued_at <= PAYLOAD_LIFETIME
                && (1..=MAX_COUNTER).contains(&payload.serial),
            "invalid directory payload"
        );
        payload.validate(history)?;
        Ok(payload)
    }
}
impl Payload {
    pub(super) fn validate(&self, history: bool) -> Result<()> {
        ensure!(
            self.nodes.len() <= MAX_NODES && self.public_stun.len() <= 128,
            "directory capacity exceeded"
        );
        let mut aliases = BTreeSet::new();
        let mut endpoints = BTreeSet::new();
        let mut stuns = BTreeSet::new();
        for node in &self.nodes {
            node.spec.validate(history)?;
            ensure!(
                aliases.insert(&node.spec.alias) && endpoints.insert(node.spec.relay_identity()?),
                "duplicate directory node"
            );
        }
        for item in &self.public_stun {
            ensure!(
                [&item.region, &item.provider, &item.failure_domain]
                    .into_iter()
                    .all(|v| valid_text(v)),
                "invalid public STUN metadata"
            );
        }
        for stun in self
            .nodes
            .iter()
            .flat_map(|n| &n.spec.stun)
            .chain(self.public_stun.iter().map(|s| &s.url))
        {
            stun_host(stun)?;
            ensure!(stuns.insert(stun), "duplicate STUN endpoint");
        }
        Ok(())
    }
    /// # Errors
    /// Rejects stale or unauthorized generations, invalid topology and exhausted serials.
    pub fn advance(
        &mut self,
        certificate: Certificate,
        grant: &Grant,
        key: &SigningKey,
        now: DateTime<Utc>,
    ) -> Result<Envelope> {
        ensure!(
            grant.generation >= self.generation
                && now >= grant.not_before
                && now < grant.expires_at
                && B64.encode(key.verifying_key().to_bytes()) == grant.key
                && grant.network == self.network,
            "unusable directory signing generation"
        );
        self.serial = if self.generation == grant.generation {
            self.serial + 1
        } else {
            1
        };
        ensure!(self.serial <= MAX_COUNTER, "directory serial exhausted");
        self.generation = grant.generation;
        self.issued_at = now;
        self.expires_at = (now + PAYLOAD_LIFETIME).min(grant.expires_at);
        self.validate(false)?;
        let bytes = serde_json::to_vec(self)?;
        let signed = key.sign(&[PAYLOAD_DOMAIN, &bytes].concat());
        Ok(Envelope {
            payload: B64.encode(bytes),
            certificate,
            signature: B64.encode(signed.to_bytes()),
        })
    }
}

#[cfg(test)]
#[path = "signature_tests.rs"]
mod tests;
