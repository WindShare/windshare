//! Operator intent and verification live in the authority transaction. Network I/O
//! starts only after the corresponding authenticated command has committed.
pub mod descriptor;
#[cfg(any(test, target_arch = "wasm32"))]
pub(crate) mod dns;
mod endpoints;
mod health;
pub mod maintenance;
pub mod monitoring;
pub mod operations;
pub mod publication;
pub mod signature;
pub mod topology;
pub mod vantage;

use crate::{
    Authority, Environment, Fault, Node, Store,
    store::{key, load, save},
};
use anyhow::{Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD as B64};
use serde::{Deserialize, Serialize};
use std::collections::BTreeSet;
use topology::{Spec, Status, public_key};

pub const EVIDENCE_SECONDS: u64 = 15 * 60;

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum NodeState {
    Disabled,
    Staging {
        check: Verification,
        enrollment: DraftEnrollment,
    },
    Listed {
        status: Status,
        #[serde(default)]
        withdrawn: BTreeSet<topology::Role>,
        #[serde(default)]
        maintenance: Option<String>,
    },
}
#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DraftEnrollment {
    Provisional,
    Disabled,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Verification {
    Queued,
    Running {
        vantage_key: String,
        lease: String,
        until: u64,
        #[serde(default)]
        failures: u8,
    },
    Failed {
        vantage_key: String,
        at: u64,
        reason: String,
        #[serde(default)]
        failures: u8,
    },
    Passed {
        vantage_key: String,
        at: u64,
    },
}
#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Promote {
    pub node_key: String,
    pub operation_id: String,
    pub status: Status,
}

pub(super) fn attach(
    store: &mut impl Store,
    spec: Spec,
    state: NodeState,
    operation_id: String,
) -> Result<Node> {
    let node = Node::new(spec, operation_id, state, 1)?;
    save(store, &key("node", &node.key), &node)?;
    Ok(node)
}

impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Rejects invalid operator sessions, conflicting identities and previously bound aliases.
    pub fn stage(&mut self, session: &str, spec: Spec) -> Result<Node> {
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
        let directory = publication::current(&self.store)?;
        directory.check_alias(&spec)?;
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
        let operation_id = B64.encode(self.environment.random::<16>()?);
        tracing::info!(event="node_staged", %operation_id, node_id=%spec.node_key, alias=%spec.alias);
        let node = self.new_staged_node(spec, operation_id)?;
        save(&mut self.store, &record, &node)?;
        Ok(node)
    }
    /// # Errors
    /// Only the named draft can be withdrawn. A stale tab cannot cancel a later enrollment.
    pub fn cancel_draft(
        &mut self,
        session: &str,
        node_key: &str,
        operation_id: &str,
    ) -> Result<()> {
        self.require_admin(session)?;
        let id = public_key(node_key)?;
        let record = key("node", &id);
        let Some(mut node) = load::<Node>(&self.store, &record)? else {
            return Ok(());
        };
        let NodeState::Staging { enrollment, .. } = node.state() else {
            return Err(Fault::Conflict {
                reason: "listing_changed".into(),
            }
            .into());
        };
        ensure!(
            node.operation_id == operation_id,
            Fault::Conflict {
                reason: "listing_changed".into()
            }
        );
        match *enrollment {
            DraftEnrollment::Disabled => {
                node.update_lifecycle(|n| {
                    *n.state = NodeState::Disabled;
                    Ok(())
                })?;
                save(&mut self.store, &record, &node)?;
            }
            DraftEnrollment::Provisional => self.retire_fleet_node(node)?,
        }
        tracing::info!(event="node_draft_cancelled", %operation_id, node_id=%node_key);
        Ok(())
    }
    /// # Errors
    /// Requires current verification and matching mirrors. Signing or storage failures abort publication.
    pub fn promote(
        &mut self,
        session: &str,
        command: &Promote,
        agents: &[vantage::Agent],
        observed_digest: &str,
        signer: &publication::Publisher,
    ) -> Result<()> {
        self.require_admin(session)?;
        let node_key = command.node_key.as_str();
        let status = command.status;
        ensure!(status != Status::Draining, Fault::Invalid);
        let now = self.environment.now();
        let record = key("node", &public_key(node_key)?);
        let mut node: Node = load(&self.store, &record)?.ok_or(Fault::NodeNotFound)?;
        ensure!(
            node.operation_id == command.operation_id,
            Fault::Conflict {
                reason: "listing_changed".into()
            }
        );
        if node.status() == Some(status) {
            return Ok(());
        }
        match node.state() {
            NodeState::Staging {
                check: Verification::Passed { at, vantage_key },
                ..
            } => ensure!(
                now.saturating_sub(*at) <= EVIDENCE_SECONDS
                    && agents
                        .iter()
                        .any(|agent| agent.key == *vantage_key && agent.zone == node.spec.zone),
                Fault::Denied {
                    reason: "verification_required".into()
                }
            ),
            NodeState::Staging { .. } | NodeState::Disabled => {
                return Err(Fault::Denied {
                    reason: "verification_required".into(),
                }
                .into());
            }
            NodeState::Listed {
                status: Status::Canary,
                ..
            } => ensure!(status == Status::Active, Fault::Invalid),
            // Draining nodes are already published and return to active service via
            // manage(Action::Resume) without blocking on mirror-freshness checks.
            NodeState::Listed {
                status: Status::Active | Status::Draining,
                ..
            } => return Err(Fault::Invalid.into()),
        }
        let mut publication = publication::current(&self.store)?;
        ensure!(
            publication::digest(&publication.envelope)? == observed_digest
                && vantage::mainland_current(&self.store, agents, observed_digest, now)?,
            Fault::Denied {
                reason: "mirror_lag".into()
            }
        );
        publication.reserve(&node.spec)?;
        node.update_lifecycle(|n| {
            match n.state {
                NodeState::Listed {
                    status: current, ..
                } => *current = status,
                NodeState::Staging { .. } | NodeState::Disabled => {
                    *n.state = NodeState::Listed {
                        status,
                        withdrawn: BTreeSet::new(),
                        maintenance: None,
                    };
                }
            }
            Ok(())
        })?;
        tracing::info!(event="node_promoted",operation_id=%node.operation_id,node_id=%node_key,?status);
        save(&mut self.store, &record, &node)?;
        publication.project(&mut self.store, signer, now, &command.operation_id)
    }
}
