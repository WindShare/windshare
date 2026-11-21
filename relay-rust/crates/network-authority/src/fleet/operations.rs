use super::{
    DraftEnrollment, Listing, ListingState, Verification,
    publication::{self, Publication, Publisher},
    topology::{Entry, Role, Status, public_key},
};
use crate::{
    Authority, Environment, Fault, Node, Store,
    store::{key, load, save},
};
use anyhow::{Result, ensure};
use base64::Engine;
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
pub use windshare_relay_access::usage::Admission;

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Manage {
    pub node_key: String,
    pub operation_id: String,
    pub action: Action,
}
#[derive(Deserialize, Serialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub enum Action {
    Drain,
    Disable { early_cutoff: bool },
    Remove { early_cutoff: bool },
    Verify,
    RestoreRole { role: Role },
}

impl Listing {
    pub(super) fn relay_drained(&self) -> bool {
        self.maintenance.is_some()
            || matches!(
                &self.state,
                ListingState::Listed { status, withdrawn }
                    if *status == Status::Draining || withdrawn.contains(&Role::Relay)
            )
    }

    pub(super) fn entry(&self) -> Option<Entry> {
        match &self.state {
            ListingState::Staging { .. } => None,
            ListingState::Disabled => {
                // The verified binary switch owns a temporary drain even when an operator
                // queues a disable. Keep only the relay alias resolvable until that
                // operation releases it; stateless STUN has no sessions to drain and must
                // not reappear after disabling a previously health-withdrawn node.
                if self.maintenance.is_none() || !self.spec.roles.contains(&Role::Relay) {
                    return None;
                }
                let mut spec = self.spec.clone();
                spec.roles.retain(|r| *r == Role::Relay);
                spec.stun.clear();
                Some(Entry {
                    spec,
                    status: Status::Draining,
                })
            }
            ListingState::Listed { status, withdrawn } => {
                let mut spec = self.spec.clone();
                if withdrawn.contains(&Role::Stun) {
                    spec.roles.retain(|r| *r != Role::Stun);
                    spec.stun.clear();
                }
                if spec.roles.is_empty() {
                    return None;
                }
                let status = if self.relay_drained() {
                    Status::Draining
                } else {
                    *status
                };
                Some(Entry { spec, status })
            }
        }
    }
}

impl Node {
    pub(super) fn apply_listing(&mut self) -> Result<()> {
        let Some(listing) = &self.listing else {
            return Ok(());
        };
        let admission = match listing.state {
            ListingState::Disabled if listing.maintenance.is_some() => Admission::Draining,
            ListingState::Disabled => Admission::Disabled,
            ListingState::Staging { .. } => Admission::Draining,
            ListingState::Listed { .. }
                if listing.relay_drained() || !listing.spec.roles.contains(&Role::Relay) =>
            {
                Admission::Draining
            }
            ListingState::Listed { .. } => Admission::Accepting,
        };
        self.set_admission(admission)
    }
}

impl Publication {
    pub(crate) fn project(
        &mut self,
        store: &mut impl Store,
        signer: &Publisher,
        now: u64,
        operation: &str,
    ) -> Result<()> {
        let mut nodes = Vec::new();
        for (_, raw) in store.scan("node/", None)? {
            let node: Node = serde_json::from_str(&raw)?;
            if let Some(entry) = node.listing.and_then(|l| l.entry()) {
                nodes.push(entry);
            }
        }
        nodes.sort_by(|a, b| a.spec.alias.cmp(&b.spec.alias));
        let mut payload = self.payload()?;
        payload
            .nodes
            .sort_by(|a, b| a.spec.alias.cmp(&b.spec.alias));
        if payload.nodes != nodes {
            let previous: BTreeMap<_, _> = payload
                .nodes
                .iter()
                .map(|n| (&n.spec.node_key, n))
                .collect();
            let next: BTreeMap<_, _> = nodes.iter().map(|n| (&n.spec.node_key, n)).collect();
            for (id, entry) in &previous {
                if !next.contains_key(id) {
                    tracing::info!(event="directory_entry_removed", operation_id=%operation, node_id=%id, alias=%entry.spec.alias);
                }
            }
            for (id, entry) in next {
                if previous.get(id).copied() != Some(entry) {
                    tracing::info!(event="directory_entry_changed", operation_id=%operation, node_id=%id,
                        alias=%entry.spec.alias, status=?entry.status, roles=?entry.spec.roles);
                }
            }
            payload.nodes = nodes;
            self.publish(payload, signer, now, operation)?;
            self.save(store)?;
        }
        Ok(())
    }
}

impl<R: Store, E: Environment> Authority<R, E> {
    pub(super) fn new_fleet_node(&self, spec: &super::topology::Spec) -> Result<Node> {
        let id = public_key(&spec.node_key)?;
        let mut node = Node::enrolled(id, spec.zone, spec.region.clone());
        if let Some(policy) = load::<windshare_relay_access::usage::NodePolicy>(
            &self.store,
            &key("retired-node", &id),
        )? {
            // The same installed node retains its durable anti-rollback barrier on re-enrollment.
            node.version = policy.version;
            node.admission = Admission::Disabled;
        } else {
            node.admission = Admission::Draining;
        }
        Ok(node)
    }

    pub(super) fn retire_fleet_node(&mut self, mut node: Node) -> Result<()> {
        node.set_admission(Admission::Disabled)?;
        save(
            &mut self.store,
            &key("retired-node", &node.key),
            &node.policy(),
        )?;
        self.store.delete(&key("node", &node.key))?;
        self.store.delete(&key("node-load", &node.key))?;
        self.live_loads.remove(&node.key);
        Ok(())
    }

    /// # Errors
    /// Requires the current listing and signed drain evidence before destructive transitions.
    pub fn manage(&mut self, session: &str, cmd: &Manage, signer: &Publisher) -> Result<()> {
        self.require_admin(session)?;
        let record = key("node", &public_key(&cmd.node_key)?);
        let mut node: Node = load(&self.store, &record)?.ok_or(Fault::NodeNotFound)?;
        let drained = self.node_details(node.clone())?.cutoff_ready;
        let listing = node.listing.as_mut().ok_or(Fault::Invalid)?;
        ensure!(
            listing.operation_id == cmd.operation_id,
            Fault::Conflict {
                reason: "listing_changed".into()
            }
        );
        match cmd.action {
            Action::Drain => {
                let ListingState::Listed { status, .. } = &mut listing.state else {
                    return Err(Fault::Invalid.into());
                };
                *status = Status::Draining;
            }
            Action::Disable { early_cutoff } => {
                ensure!(
                    early_cutoff || drained,
                    Fault::Conflict {
                        reason: "drain_pending".into()
                    }
                );
                listing.state = ListingState::Disabled;
            }
            Action::Remove { early_cutoff } => {
                ensure!(
                    matches!(listing.state, ListingState::Disabled)
                        && listing.maintenance.is_none(),
                    Fault::Invalid
                );
                ensure!(
                    early_cutoff || drained,
                    Fault::Conflict {
                        reason: "drain_pending".into()
                    }
                );
                self.retire_fleet_node(node)?;
                tracing::info!(event="node_removed", operation_id=%cmd.operation_id,node_id=%cmd.node_key,early_cutoff);
                return Ok(());
            }
            Action::Verify => {
                ensure!(
                    matches!(listing.state, ListingState::Disabled)
                        && listing.maintenance.is_none(),
                    Fault::Invalid
                );
                listing.operation_id = super::B64.encode(self.environment.random::<16>()?);
                listing.state = ListingState::Staging {
                    check: Verification::Queued,
                    enrollment: DraftEnrollment::DisabledListing,
                };
            }
            Action::RestoreRole { role } => {
                let ListingState::Listed { withdrawn, .. } = &mut listing.state else {
                    return Err(Fault::Invalid.into());
                };
                ensure!(listing.spec.roles.contains(&role), Fault::Invalid);
                withdrawn.remove(&role);
                super::monitoring::reset_evidence(&mut self.store, &cmd.node_key, role)?;
            }
        }
        node.apply_listing()?;
        save(&mut self.store, &record, &node)?;
        tracing::info!(event="node_intent_changed", operation_id=%cmd.operation_id,node_id=%cmd.node_key, admission=?node.admission,version=node.version);
        publication::current(&self.store)?.project(
            &mut self.store,
            signer,
            self.environment.now(),
            &cmd.operation_id,
        )
    }
}
