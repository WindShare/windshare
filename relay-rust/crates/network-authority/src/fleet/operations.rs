use super::{
    NodeState,
    publication::{self, Publication, Publisher},
    topology::{Entry, Role, public_key},
};
use crate::{
    Authority, Environment, Fault, Node, Store,
    store::{key, load, save},
};
use anyhow::{Result, ensure};
use base64::Engine;
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

#[derive(Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Manage {
    pub node_key: String,
    pub expected_state: NodeState,
    pub expected_policy_version: u64,
    pub action: Action,
}

#[derive(Clone, Copy, Debug, Deserialize, Serialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub enum Action {
    Activate,
    Drain,
    Disable { early_cutoff: bool },
    Remove { early_cutoff: bool },
}

impl Node {
    fn entry(&self) -> Option<Entry> {
        Some(Entry {
            spec: self.spec.clone(),
            status: self.status()?,
        })
    }
}

impl Publication {
    pub(crate) fn project(
        &mut self,
        store: &mut impl Store,
        signer: &Publisher,
        now: u64,
        operation_id: &str,
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
                    tracing::info!(event="directory_entry_removed", %operation_id, node_id=%id, alias=%entry.spec.alias);
                }
            }
            for (id, entry) in next {
                if previous.get(id).copied() != Some(entry) {
                    tracing::info!(event="directory_entry_changed", %operation_id, node_id=%id,
                        alias=%entry.spec.alias, status=?entry.status, roles=?entry.spec.roles);
                }
            }
            payload.nodes = nodes;
            self.publish(payload, signer, now, operation_id)?;
            self.save(store)?;
        }
        Ok(())
    }
}

impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Rejects stale views and destructive changes without fresh drain evidence or an explicit cutoff.
    pub fn manage(&mut self, session: &str, cmd: &Manage, signer: &Publisher) -> Result<()> {
        self.require_admin(session)?;
        let record = key("node", &public_key(&cmd.node_key)?);
        let stale = || Fault::Conflict {
            reason: "listing_changed".into(),
        };
        let mut node: Node = load(&self.store, &record)?.ok_or_else(stale)?;
        ensure!(
            node.state() == cmd.expected_state && node.version == cmd.expected_policy_version,
            stale()
        );
        let operation_id = super::B64.encode(self.environment.random::<16>()?);
        let next = match cmd.action {
            Action::Activate => NodeState::Active,
            Action::Drain => {
                ensure!(
                    node.state() != NodeState::Disabled && node.spec.roles.contains(&Role::Relay),
                    Fault::Invalid
                );
                NodeState::Draining
            }
            Action::Disable { early_cutoff } | Action::Remove { early_cutoff } => {
                let removing = matches!(cmd.action, Action::Remove { .. });
                if removing {
                    ensure!(node.state() == NodeState::Disabled, Fault::Invalid);
                } else if node.state() == NodeState::Disabled {
                    return Ok(());
                }
                ensure!(
                    early_cutoff || self.node_details(node.clone())?.cutoff_ready,
                    Fault::Conflict {
                        reason: "drain_pending".into()
                    }
                );
                if removing {
                    save(
                        &mut self.store,
                        &key("retired-node", &node.key),
                        &node.policy(),
                    )?;
                    self.store.delete(&record)?;
                    self.store.delete(&key("node-load", &node.key))?;
                    self.live_loads.remove(&node.key);
                    tracing::info!(event="node_removed", %operation_id, node_id=%cmd.node_key, early_cutoff);
                    return Ok(());
                }
                NodeState::Disabled
            }
        };
        if node.state() != next {
            node.set_state(next)?;
            save(&mut self.store, &record, &node)?;
            publication::current(&self.store)?.project(
                &mut self.store,
                signer,
                self.environment.now(),
                &operation_id,
            )?;
            tracing::info!(event="node_intent_changed", %operation_id, node_id=%cmd.node_key, action=?cmd.action, admission=?node.admission(), version=node.version);
        }
        Ok(())
    }
}
