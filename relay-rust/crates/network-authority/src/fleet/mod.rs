//! Operator intent and verification live in the authority transaction. Network I/O
//! starts only after the corresponding authenticated command has committed.
pub mod descriptor;
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
use signature::Envelope;
use topology::{Spec, Status, public_key};

pub const EVIDENCE_SECONDS: u64 = 15 * 60;

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct Listing {
    pub spec: Spec,
    pub operation_id: String,
    pub state: ListingState,
}
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum ListingState {
    Staging { check: Verification },
    Listed { status: Status },
}
#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Verification {
    Queued,
    Running {
        vantage_key: String,
        lease: String,
        until: u64,
    },
    Failed {
        vantage_key: String,
        at: u64,
        reason: String,
    },
    Passed {
        vantage_key: String,
        at: u64,
    },
}
impl Listing {
    pub const fn status(&self) -> Option<Status> {
        match self.state {
            ListingState::Listed { status } => Some(status),
            ListingState::Staging { .. } => None,
        }
    }
}

pub(super) fn attach(
    store: &mut impl Store,
    spec: Spec,
    state: ListingState,
    operation_id: String,
) -> Result<Node> {
    let id = public_key(&spec.node_key)?;
    let record = key("node", &id);
    let mut node = load::<Node>(store, &record)?
        .unwrap_or_else(|| Node::enrolled(id, spec.zone, spec.region.clone()));
    node.zone = spec.zone;
    node.region.clone_from(&spec.region);
    node.listing = Some(Listing {
        spec,
        operation_id,
        state,
    });
    save(store, &record, &node)?;
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
        if let Some(node) = load::<Node>(&self.store, &record)?
            && let Some(listing) = &node.listing
        {
            ensure!(listing.spec == spec, Fault::Invalid);
            return Ok(node);
        }
        let rows = self.store.scan("node/", None)?;
        let mut count = 0;
        for (_, value) in rows {
            if let Some(listing) = serde_json::from_str::<Node>(&value)?.listing {
                ensure!(
                    listing.spec.alias != spec.alias && listing.spec.relay != spec.relay,
                    Fault::Denied {
                        reason: "alias_bound".into()
                    }
                );
                count += 1;
            }
        }
        ensure!(count < topology::MAX_NODES, Fault::Invalid);
        let mut directory = publication::current(&self.store)?;
        directory.reserve(&spec)?;
        let operation_id = B64.encode(self.environment.random::<16>()?);
        tracing::info!(event="node_staged", %operation_id, node_id=%spec.node_key, alias=%spec.alias);
        let node = attach(
            &mut self.store,
            spec,
            ListingState::Staging {
                check: Verification::Queued,
            },
            operation_id,
        )?;
        directory.save(&mut self.store)?;
        Ok(node)
    }
    /// # Errors
    /// Requires current verification and matching mirrors. Signing or storage failures abort publication.
    pub fn promote(
        &mut self,
        session: &str,
        node_key: &str,
        status: Status,
        agents: &[vantage::Agent],
        observed: &Envelope,
        signer: &publication::Publisher,
    ) -> Result<()> {
        self.require_admin(session)?;
        ensure!(status != Status::Draining, Fault::Invalid);
        let now = self.environment.now();
        let record = key("node", &public_key(node_key)?);
        let mut node: Node = load(&self.store, &record)?.ok_or(Fault::NodeNotFound)?;
        let listing = node.listing.as_mut().ok_or(Fault::Invalid)?;
        if listing.status() == Some(status) {
            return Ok(());
        }
        let eligible = match &listing.state {
            ListingState::Staging {
                check: Verification::Passed { at, vantage_key },
            } => {
                now.saturating_sub(*at) <= EVIDENCE_SECONDS
                    && agents
                        .iter()
                        .any(|agent| agent.key == *vantage_key && agent.zone == node.zone)
            }
            ListingState::Listed {
                status: Status::Canary,
            } => status == Status::Active,
            _ => false,
        };
        ensure!(
            eligible,
            Fault::Denied {
                reason: "verification_required".into()
            }
        );
        ensure!(node.enabled, Fault::Invalid);
        let mut publication = publication::current(&self.store)?;
        ensure!(
            publication.envelope == *observed
                && vantage::mainland_current(&self.store, agents, observed, now)?,
            Fault::Denied {
                reason: "mirror_lag".into()
            }
        );
        let mut payload = publication.payload()?;
        payload.nodes.retain(|n| n.spec.node_key != node_key);
        payload.nodes.push(topology::Entry {
            spec: listing.spec.clone(),
            status,
        });
        payload
            .nodes
            .sort_by(|a, b| a.spec.alias.cmp(&b.spec.alias));
        publication.publish(payload, signer, now, &listing.operation_id)?;
        listing.state = ListingState::Listed { status };
        tracing::info!(event="node_promoted",operation_id=%listing.operation_id,node_id=%node_key,?status);
        save(&mut self.store, &record, &node)?;
        publication.save(&mut self.store)
    }
}
