use super::{
    DraftEnrollment, NodeState, Verification,
    publication::{self, Publication, Publisher},
    topology::{Entry, Role, Status, public_key},
};
use crate::{
    Authority, Environment, Fault, Node, Store,
    nodes::NodeMutation,
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
#[derive(Debug, Deserialize, Serialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub enum Action {
    Drain,
    Resume,
    Disable { early_cutoff: bool },
    Remove { early_cutoff: bool },
    Verify,
    AbortMaintenance { maintenance_id: String },
}

fn resume_state(state: &mut NodeState) -> Result<bool> {
    match state {
        NodeState::Listed {
            status: status @ Status::Draining,
            ..
        } => {
            *status = Status::Active;
            Ok(true)
        }
        // Resuming an already-active node is idempotent so retries and
        // concurrent admin tabs converge cleanly. Canary rollouts must advance
        // through promote() so mirror freshness is verified first.
        NodeState::Listed {
            status: Status::Active,
            ..
        } => Ok(false),
        NodeState::Listed {
            status: Status::Canary,
            ..
        }
        | NodeState::Staging { .. }
        | NodeState::Disabled => Err(Fault::Invalid.into()),
    }
}

impl Node {
    pub(super) fn entry(&self) -> Option<Entry> {
        match self.state() {
            NodeState::Staging { .. } | NodeState::Disabled => None,
            NodeState::Listed {
                status,
                maintenance,
            } => {
                let mut spec = self.spec.clone();
                // Stateless STUN has no sessions to drain; omit it while a verified
                // binary switch holds maintenance.
                if maintenance.is_some() {
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
            if let Some(entry) = node.entry() {
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
    pub(super) fn new_staged_node(
        &self,
        spec: super::topology::Spec,
        operation_id: String,
    ) -> Result<Node> {
        let id = public_key(&spec.node_key)?;
        let version = if let Some(policy) = load::<windshare_relay_access::usage::NodePolicy>(
            &self.store,
            &key("retired-node", &id),
        )? {
            // Retired nodes were frozen at Admission::Disabled; staging enters
            // Admission::Draining, so advance the anti-rollback barrier explicitly.
            policy.version.checked_add(1).ok_or(Fault::Invalid)?
        } else {
            1
        };
        Node::new(
            spec,
            operation_id,
            NodeState::Staging {
                check: Verification::Queued,
                enrollment: DraftEnrollment::Provisional,
            },
            version,
        )
    }

    pub(super) fn retire_fleet_node(&mut self, mut node: Node) -> Result<()> {
        let policy = node.retired_policy()?;
        save(&mut self.store, &key("retired-node", &node.key), &policy)?;
        self.store.delete(&key("node", &node.key))?;
        self.store.delete(&key("node-load", &node.key))?;
        self.live_loads.remove(&node.key);
        Ok(())
    }

    fn apply_manage_action(
        &mut self,
        node: &mut NodeMutation<'_>,
        cmd: &Manage,
        drained: bool,
    ) -> Result<bool> {
        match &cmd.action {
            Action::Drain => {
                let NodeState::Listed { status, .. } = node.state else {
                    return Err(Fault::Invalid.into());
                };
                *status = Status::Draining;
            }
            Action::Resume => return resume_state(node.state),
            &Action::Disable { early_cutoff } => match node.state {
                // Refuse crossing out of Listed while an active upgrade holds the
                // maintenance lock so Disabled stays strictly lock-free and final.
                NodeState::Listed {
                    maintenance: Some(_),
                    ..
                } => {
                    return Err(Fault::Conflict {
                        reason: "maintenance_in_progress".into(),
                    }
                    .into());
                }
                NodeState::Listed {
                    maintenance: None, ..
                } => {
                    ensure!(
                        early_cutoff || drained,
                        Fault::Conflict {
                            reason: "drain_pending".into()
                        }
                    );
                    *node.state = NodeState::Disabled;
                }
                // Disabling an already-disabled node is idempotent so retries and
                // concurrent admin tabs converge without tripping a stale drain check.
                NodeState::Disabled => return Ok(false),
                // Staging drafts carry rollback metadata and must leave via promote or cancel_draft.
                NodeState::Staging { .. } => return Err(Fault::Invalid.into()),
            },
            Action::Verify => {
                ensure!(matches!(node.state, NodeState::Disabled), Fault::Invalid);
                *node.operation_id = super::B64.encode(self.environment.random::<16>()?);
                *node.state = NodeState::Staging {
                    check: Verification::Queued,
                    enrollment: DraftEnrollment::Disabled,
                };
            }
            Action::AbortMaintenance { maintenance_id } => {
                super::maintenance::abort(
                    &mut self.store,
                    node,
                    &cmd.operation_id,
                    maintenance_id,
                )?;
            }
            Action::Remove { .. } => unreachable!("remove handled before lifecycle mutation"),
        }
        Ok(true)
    }

    /// # Errors
    /// Requires the current node operation ID and signed drain evidence before destructive transitions.
    pub fn manage(&mut self, session: &str, cmd: &Manage, signer: &Publisher) -> Result<()> {
        self.require_admin(session)?;
        let record = key("node", &public_key(&cmd.node_key)?);
        let mut node: Node = load(&self.store, &record)?.ok_or(Fault::NodeNotFound)?;
        let drained = self.node_details(node.clone())?.cutoff_ready;
        ensure!(
            node.operation_id == cmd.operation_id,
            Fault::Conflict {
                reason: "listing_changed".into()
            }
        );
        if let &Action::Remove { early_cutoff } = &cmd.action {
            ensure!(matches!(node.state(), NodeState::Disabled), Fault::Invalid);
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
        let changed = node.update_lifecycle(|n| self.apply_manage_action(n, cmd, drained))?;
        if !changed {
            return Ok(());
        }
        save(&mut self.store, &record, &node)?;
        tracing::info!(event="node_intent_changed", operation_id=%cmd.operation_id,node_id=%cmd.node_key, action=?cmd.action, admission=?node.admission(),version=node.version);
        publication::current(&self.store)?.project(
            &mut self.store,
            signer,
            self.environment.now(),
            &cmd.operation_id,
        )
    }
}
