//! Fleet mutations and their signed directory projection share one authority transaction.
pub mod descriptor;
#[cfg(any(test, target_arch = "wasm32"))]
pub(crate) mod dns;
mod endpoints;
pub mod operations;
pub mod publication;
pub mod signature;
pub mod topology;

use crate::{
    Authority, Environment, Fault, Node, Store,
    store::{key, load, save},
};
use anyhow::{Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD as B64};
use serde::{Deserialize, Serialize};
use topology::{Spec, Status, public_key};

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum NodeState {
    Canary,
    Active,
    Draining,
    Disabled,
}

impl From<Status> for NodeState {
    fn from(status: Status) -> Self {
        match status {
            Status::Canary => Self::Canary,
            Status::Active => Self::Active,
            Status::Draining => Self::Draining,
        }
    }
}

impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Rejects invalid operator sessions, conflicting identities and previously bound aliases.
    pub fn enroll(
        &mut self,
        session: &str,
        spec: Spec,
        signer: &publication::Publisher,
    ) -> Result<Node> {
        self.require_admin(session)?;
        spec.validate(false)?;
        ensure!(
            topology::public_origin(&spec.relay)? == spec.relay && spec.operator == "official",
            Fault::Invalid
        );
        let record = key("node", &public_key(&spec.node_key)?);
        if let Some(node) = load::<Node>(&self.store, &record)? {
            ensure!(
                node.spec == spec,
                Fault::Conflict {
                    reason: "listing_changed".into()
                }
            );
            return Ok(node);
        }
        let mut directory = publication::current(&self.store)?;
        directory.reserve(&spec)?;
        let mut occupied = topology::Occupied::default();
        for stun in directory.payload()?.public_stun {
            occupied.add_stun(&stun.url)?;
        }
        let mut count = 0;
        for (_, value) in self.store.scan("node/", None)? {
            let node: Node = serde_json::from_str(&value)?;
            occupied.add(&node.spec)?;
            count += 1;
        }
        ensure!(count < topology::MAX_NODES, Fault::Invalid);
        occupied.add(&spec)?;
        let version = load::<windshare_relay_access::usage::NodePolicy>(
            &self.store,
            &key("retired-node", &public_key(&spec.node_key)?),
        )?
        .map_or(Ok(1), |policy| {
            policy.version.checked_add(1).ok_or(Fault::Invalid)
        })?;
        let node = Node::new(spec, NodeState::Canary, version)?;
        let operation_id = B64.encode(self.environment.random::<16>()?);
        save(&mut self.store, &record, &node)?;
        directory.project(
            &mut self.store,
            signer,
            self.environment.now(),
            &operation_id,
        )?;
        tracing::info!(event="node_enrolled", %operation_id, node_id=%node.spec.node_key, alias=%node.spec.alias, version);
        Ok(node)
    }
}
