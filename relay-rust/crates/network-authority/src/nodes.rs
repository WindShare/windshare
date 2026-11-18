use crate::{
    Authority, Environment, Fault, Store,
    store::{key, load, save},
};
use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize};

const MAX_REGION_BYTES: usize = 128;

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum Zone {
    Global,
    Cn,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NodeEnrollment {
    pub key: [u8; 32],
    pub zone: Zone,
    pub region: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Node {
    pub key: [u8; 32],
    pub zone: Zone,
    pub region: String,
    pub enabled: bool,
    pub anonymous: bool,
    #[serde(default)]
    pub version: u64,
}

#[derive(Clone, Copy, Debug, Deserialize)]
#[serde(
    tag = "field",
    content = "value",
    rename_all = "snake_case",
    deny_unknown_fields
)]
pub enum NodePolicyChange {
    PublicRelay(bool),
    AnonymousSharing(bool),
}

impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Requires an administrator session, a nonzero public key and a named region.
    pub fn enroll_node(&mut self, session: &str, enrollment: &NodeEnrollment) -> Result<Node> {
        self.require_admin(session)?;
        let region = enrollment.region.trim();
        ensure!(
            enrollment.key != [0; 32] && !region.is_empty() && region.len() <= MAX_REGION_BYTES,
            Fault::Invalid
        );
        let record = key("node", &enrollment.key);
        if let Some(node) = load(&self.store, &record)? {
            tracing::info!(event = "node_already_enrolled", node = record);
            return Ok(node);
        }
        let node = Node {
            key: enrollment.key,
            zone: enrollment.zone,
            region: region.to_owned(),
            enabled: true,
            anonymous: enrollment.zone == Zone::Global,
            version: 1,
        };
        save(&mut self.store, &record, &node)?;
        tracing::info!(event = "node_enrolled", node = record, zone = ?node.zone, version = node.version);
        Ok(node)
    }

    /// # Errors
    /// Requires a live administrator session and an enrolled node with a writable revision.
    pub fn set_node_policy(
        &mut self,
        session: &str,
        public_key: &[u8; 32],
        change: NodePolicyChange,
    ) -> Result<Node> {
        self.require_admin(session)?;
        let record = key("node", public_key);
        let mut node: Node = load(&self.store, &record)?.ok_or(Fault::Invalid)?;
        let (field, value) = match change {
            NodePolicyChange::PublicRelay(value) => (&mut node.enabled, value),
            NodePolicyChange::AnonymousSharing(value) => (&mut node.anonymous, value),
        };
        // A lost response can be retried without publishing a fictitious new revision.
        if *field != value {
            *field = value;
            node.version = node.version.checked_add(1).ok_or(Fault::Invalid)?;
            save(&mut self.store, &record, &node)?;
        }
        tracing::info!(
            event = "node_policy_saved",
            node = record,
            ?change,
            version = node.version
        );
        Ok(node)
    }
}
