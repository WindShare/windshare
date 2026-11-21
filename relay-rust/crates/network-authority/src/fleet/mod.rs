//! Operator intent and verification live in the authority transaction. Network I/O
//! starts only after the corresponding authenticated command has committed.
pub mod descriptor;
#[cfg(any(test, target_arch = "wasm32"))]
pub(crate) mod dns;
mod endpoints;
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
    Staging {
        check: Verification,
        #[serde(default)]
        enrollment: DraftEnrollment,
    },
    Listed {
        status: Status,
    },
}
#[derive(Clone, Copy, Debug, Default, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DraftEnrollment {
    #[default]
    Legacy,
    Existing,
    Provisional,
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
        let existing: Option<Node> = load(&self.store, &record)?;
        if let Some(node) = &existing
            && let Some(listing) = &node.listing
        {
            ensure!(
                listing.spec == spec,
                Fault::Conflict {
                    reason: "listing_changed".into()
                }
            );
            return Ok(node.clone());
        }
        let directory = publication::current(&self.store)?;
        directory.check_alias(&spec)?;
        let mut occupied = topology::Occupied::default();
        for stun in directory.payload()?.public_stun {
            occupied.add_stun(&stun.url)?;
        }
        let mut count = 0;
        for (_, value) in self.store.scan("node/", None)? {
            if let Some(listing) = serde_json::from_str::<Node>(&value)?.listing {
                occupied.add(&listing.spec)?;
                count += 1;
            }
        }
        ensure!(count < topology::MAX_NODES, Fault::Invalid);
        occupied.add(&spec)?;
        let (mut node, enrollment) = match existing {
            Some(node) => (node, DraftEnrollment::Existing),
            None => (
                Node::enrolled(public_key(&spec.node_key)?, spec.zone, spec.region.clone()),
                DraftEnrollment::Provisional,
            ),
        };
        let operation_id = B64.encode(self.environment.random::<16>()?);
        tracing::info!(event="node_staged", %operation_id, node_id=%spec.node_key, alias=%spec.alias);
        node.listing = Some(Listing {
            spec,
            operation_id,
            state: ListingState::Staging {
                check: Verification::Queued,
                enrollment,
            },
        });
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
        let Some(listing) = node.listing.take() else {
            return Ok(());
        };
        let ListingState::Staging { enrollment, .. } = listing.state else {
            return Err(Fault::Conflict {
                reason: "listing_changed".into(),
            }
            .into());
        };
        ensure!(
            listing.operation_id == operation_id,
            Fault::Conflict {
                reason: "listing_changed".into()
            }
        );
        if matches!(enrollment, DraftEnrollment::Legacy) {
            publication::withdraw_legacy_alias(&mut self.store, &listing.spec.alias)?;
        }
        match enrollment {
            DraftEnrollment::Existing | DraftEnrollment::Legacy => {
                save(&mut self.store, &record, &node)?;
            }
            DraftEnrollment::Provisional => {
                self.store.delete(&record)?;
                self.store.delete(&key("node-load", &id))?;
                self.live_loads.remove(&id);
            }
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
        let listing = node.listing.as_mut().ok_or(Fault::Invalid)?;
        ensure!(
            listing.operation_id == command.operation_id,
            Fault::Conflict {
                reason: "listing_changed".into()
            }
        );
        if listing.status() == Some(status) {
            return Ok(());
        }
        let eligible = match &listing.state {
            ListingState::Staging {
                check: Verification::Passed { at, vantage_key },
                ..
            } => {
                now.saturating_sub(*at) <= EVIDENCE_SECONDS
                    && agents
                        .iter()
                        .any(|agent| agent.key == *vantage_key && agent.zone == listing.spec.zone)
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
            publication::digest(&publication.envelope)? == observed_digest
                && vantage::mainland_current(&self.store, agents, observed_digest, now)?,
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
        publication.reserve(&listing.spec)?;
        publication.publish(payload, signer, now, &listing.operation_id)?;
        node.zone = listing.spec.zone;
        node.region.clone_from(&listing.spec.region);
        listing.state = ListingState::Listed { status };
        tracing::info!(event="node_promoted",operation_id=%listing.operation_id,node_id=%node_key,?status);
        save(&mut self.store, &record, &node)?;
        publication.save(&mut self.store)
    }
}
