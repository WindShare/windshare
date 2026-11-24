use super::{
    signature::{Certificate, Envelope, Grant, Payload, RENEW_BEFORE, Root},
    topology::Spec,
};
use crate::{
    Fault, Node, Store,
    store::{key, load, save},
};
use anyhow::{Context, Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD as B64};
use chrono::{DateTime, Utc};
use ed25519_dalek::SigningKey;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::collections::BTreeMap;

const DIRECTORY: &str = "directory/current";
pub const KV_KEY: &str = "windshare-network";
const HISTORY: &[&str] = include!(concat!(env!("OUT_DIR"), "/directory_history.rs"));

#[derive(Deserialize)]
pub struct Release {
    pub network: String,
    pub roots: Vec<Root>,
    pub snapshot: Envelope,
    pub link_host: String,
    pub backup_entry: String,
}
impl Release {
    /// # Errors
    /// Returns an error if the embedded release manifest cannot be decoded.
    pub fn bundled() -> Result<Self> {
        Ok(serde_json::from_str(include_str!(
            "../../../../../deploy/network/release.json"
        ))?)
    }
}
pub struct Publisher {
    roots: Vec<Root>,
    cert: Certificate,
    grant: Grant,
    key: SigningKey,
}
impl Publisher {
    /// # Errors
    /// Rejects an unauthorized certificate or a seed that does not match its certified key.
    pub fn new(release: &Release, cert: Certificate, seed: &str) -> Result<Self> {
        let grant = cert.verify(&release.network, &release.roots)?;
        let bytes: [u8; 32] = B64
            .decode(seed)?
            .try_into()
            .map_err(|_| anyhow::anyhow!("directory seed width"))?;
        let key = SigningKey::from_bytes(&bytes);
        ensure!(
            B64.encode(key.verifying_key().to_bytes()) == grant.key,
            "directory signing key mismatch"
        );
        Ok(Self {
            roots: release.roots.clone(),
            cert,
            grant,
            key,
        })
    }
    /// # Errors
    /// Rejects a stale, conflicting or not-yet-authorized publishing key during deploy preflight.
    pub fn check_release(&self, release: &Release, now: u64) -> Result<()> {
        let snapshot = release
            .snapshot
            .verify(&release.network, &release.roots, false)?;
        let time = timestamp(now)?;
        ensure!(
            time >= self.grant.not_before
                && time < self.grant.expires_at
                && (self.grant.generation > snapshot.generation
                    || self.grant.generation == snapshot.generation
                        && release.snapshot.certificate == self.cert),
            "directory generation cannot publish this release"
        );
        Ok(())
    }
}

#[derive(Serialize, Deserialize)]
pub struct Publication {
    pub envelope: Envelope,
    aliases: BTreeMap<String, String>,
}
#[derive(Serialize)]
pub struct Revision {
    pub generation: u64,
    pub serial: u64,
    pub expires: i64,
    pub keys: Vec<String>,
    pub digest: String,
    pub certificate_expires: i64,
}
/// # Errors
/// Rejects payloads that are not base64url encoded.
pub fn digest(envelope: &Envelope) -> Result<String> {
    Ok(B64.encode(Sha256::digest(B64.decode(&envelope.payload)?)))
}
/// # Errors
/// Fails when storage is unavailable or the release has not been imported.
pub fn current(store: &impl Store) -> Result<Publication> {
    load(store, DIRECTORY)?.context("directory has not been imported")
}

impl Publication {
    /// # Errors
    /// Rejects invalid signatures, conflicting revisions, alias reassignments and storage failures.
    pub fn bootstrap(store: &mut impl Store, release: &Release, history: &[&str]) -> Result<Self> {
        let payload = release
            .snapshot
            .verify(&release.network, &release.roots, false)?;
        let mut next = Self {
            envelope: release.snapshot.clone(),
            aliases: BTreeMap::new(),
        };
        let mut previous = None;
        for raw in history {
            let envelope = Envelope::decode(raw.as_bytes())?;
            let past = envelope.verify(&release.network, &release.roots, true)?;
            let cursor = (past.generation, past.serial);
            ensure!(
                previous.is_none_or(|p| p < cursor)
                    && cursor <= (payload.generation, payload.serial),
                "release history cursor conflict"
            );
            if cursor == (payload.generation, payload.serial) {
                ensure!(
                    envelope.payload == release.snapshot.payload,
                    "release equivocation"
                );
            }
            previous = Some(cursor);
            for entry in past.nodes {
                next.reserve(&entry.spec)?;
            }
        }
        for entry in &payload.nodes {
            next.reserve(&entry.spec)?;
        }
        for entry in payload.nodes {
            let node = Node::new(entry.spec, entry.status.into(), 1)?;
            save(store, &key("node", &node.key), &node)?;
        }
        tracing::info!(
            event = "directory_imported",
            operation_id = "release-import",
            generation = payload.generation,
            serial = payload.serial,
            aliases = next.aliases.len()
        );
        next.save(store)?;
        Ok(next)
    }
    /// # Errors
    /// Propagates storage and signed release verification errors.
    pub fn load(store: &impl Store) -> Result<Option<Self>> {
        load(store, DIRECTORY)
    }

    /// # Errors
    /// Propagates storage and signed release verification errors.
    pub fn import_bundled(store: &mut impl Store) -> Result<Self> {
        Self::bootstrap(store, &Release::bundled()?, HISTORY)
    }
    pub(super) fn check_alias(&self, spec: &Spec) -> Result<()> {
        let endpoint = spec.relay_identity()?;
        ensure!(
            self.aliases
                .get(&spec.alias)
                .is_none_or(|prev| prev == &endpoint),
            Fault::Conflict {
                reason: "alias_bound".into()
            }
        );
        Ok(())
    }
    pub(super) fn reserve(&mut self, spec: &Spec) -> Result<()> {
        self.check_alias(spec)?;
        self.aliases
            .insert(spec.alias.clone(), spec.relay_identity()?);
        Ok(())
    }
    /// # Errors
    /// Fails if the stored signed payload is corrupt.
    pub fn payload(&self) -> Result<Payload> {
        // This row can only be imported from a verified release or signed in the same transaction.
        Ok(serde_json::from_slice(
            &B64.decode(&self.envelope.payload)?,
        )?)
    }
    /// # Errors
    /// Fails if the stored envelope or its certificate is corrupt.
    pub fn revision(&self) -> Result<Revision> {
        let data = self.payload()?;
        let cert: Grant = serde_json::from_slice(&B64.decode(&self.envelope.certificate.payload)?)?;
        Ok(Revision {
            generation: data.generation,
            serial: data.serial,
            expires: data.expires_at.timestamp().min(cert.expires_at.timestamp()),
            keys: data.nodes.into_iter().map(|n| n.spec.node_key).collect(),
            digest: digest(&self.envelope)?,
            certificate_expires: cert.expires_at.timestamp(),
        })
    }
    /// # Errors
    /// Rejects an unusable signing generation or a corrupt stored payload.
    pub fn renew(&mut self, signer: &Publisher, now: u64) -> Result<bool> {
        let data = self.payload()?;
        let time = timestamp(now)?;
        // A short remaining certificate window cannot be extended by signing the same expiry every tick.
        if data.generation == signer.grant.generation
            && (data.expires_at - time > RENEW_BEFORE || data.expires_at >= signer.grant.expires_at)
        {
            return Ok(false);
        }
        self.publish(data, signer, now, "renewal")?;
        Ok(true)
    }
    pub(super) fn publish(
        &mut self,
        mut data: Payload,
        signer: &Publisher,
        now: u64,
        operation_id: &str,
    ) -> Result<()> {
        ensure!(
            data.generation != signer.grant.generation || self.envelope.certificate == signer.cert,
            "conflicting directory generation"
        );
        self.envelope = data.advance(
            signer.cert.clone(),
            &signer.grant,
            &signer.key,
            &signer.roots,
            timestamp(now)?,
        )?;
        tracing::info!(event="directory_signed",%operation_id,generation=data.generation,serial=data.serial,nodes=data.nodes.len());
        Ok(())
    }
    /// # Errors
    /// Propagates serialization and storage errors.
    pub fn save(&self, store: &mut impl Store) -> Result<()> {
        save(store, DIRECTORY, self)
    }
}
fn timestamp(now: u64) -> Result<DateTime<Utc>> {
    DateTime::from_timestamp(i64::try_from(now)?, 0).context("invalid signing time")
}
