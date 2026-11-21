use crate::{
    Authority, Environment, Fault, Store,
    fleet::{
        NodeState,
        topology::{Role, Spec, Status, public_key},
    },
    store::{key, load, save},
};
use anyhow::Result;
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;
use windshare_relay_access::usage::{Admission, Load, NodePolicy, REPORT_INTERVAL_SECONDS};

const NODE_PAGE_SIZE: usize = 1000;
pub const NODE_LOAD_CHECKPOINT_SECONDS: u64 = 15 * 60;

#[derive(Clone, Copy, Debug, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "lowercase")]
pub enum Zone {
    Global,
    Cn,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Node {
    pub key: [u8; 32],
    pub spec: Spec,
    pub operation_id: String,
    state: NodeState,
    #[serde(default)]
    maintenance: Option<String>,
    pub anonymous: bool,
    pub version: u64,
}

pub struct NodeMutation<'a> {
    pub spec: &'a Spec,
    pub operation_id: &'a mut String,
    pub state: &'a mut NodeState,
    pub maintenance: &'a mut Option<String>,
}

impl Node {
    pub(crate) fn new(
        spec: Spec,
        operation_id: String,
        state: NodeState,
        version: u64,
    ) -> Result<Self> {
        let key = public_key(&spec.node_key)?;
        let anonymous = spec.zone == Zone::Global;
        Ok(Self {
            key,
            spec,
            operation_id,
            state,
            maintenance: None,
            anonymous,
            version,
        })
    }

    pub const fn state(&self) -> &NodeState {
        &self.state
    }

    pub fn maintenance(&self) -> Option<&str> {
        self.maintenance.as_deref()
    }

    pub const fn status(&self) -> Option<Status> {
        match self.state {
            NodeState::Listed { status, .. } => Some(status),
            NodeState::Staging { .. } | NodeState::Disabled => None,
        }
    }

    pub fn relay_drained(&self) -> bool {
        self.maintenance.is_some()
            || matches!(
                &self.state,
                NodeState::Listed { status, withdrawn }
                    if *status == Status::Draining || withdrawn.contains(&Role::Relay)
            )
    }

    pub fn admission(&self) -> Admission {
        match self.state {
            NodeState::Disabled if self.maintenance.is_some() => Admission::Draining,
            NodeState::Disabled => Admission::Disabled,
            NodeState::Staging { .. } => Admission::Draining,
            NodeState::Listed { .. }
                if self.relay_drained() || !self.spec.roles.contains(&Role::Relay) =>
            {
                Admission::Draining
            }
            NodeState::Listed { .. } => Admission::Accepting,
        }
    }

    pub(crate) fn policy(&self) -> NodePolicy {
        NodePolicy {
            version: self.version,
            admission: self.admission(),
            anonymous: self.anonymous,
            probe_account: windshare_relay_access::probe::PROBE_ACCOUNT,
        }
    }

    /// Produces the final disabled policy for a retired node record, advancing
    /// `version` only when retirement changes the effective relay admission.
    pub(crate) fn retired_policy(&mut self) -> Result<NodePolicy> {
        if self.admission() != Admission::Disabled {
            self.bump_version()?;
        }
        Ok(NodePolicy {
            version: self.version,
            admission: Admission::Disabled,
            anonymous: self.anonymous,
            probe_account: windshare_relay_access::probe::PROBE_ACCOUNT,
        })
    }

    /// Mutates fleet lifecycle fields and advances `version` whenever the
    /// derived relay `Admission` changes, preserving the anti-rollback barrier.
    pub(crate) fn update_lifecycle<T>(
        &mut self,
        mutate: impl FnOnce(&mut NodeMutation<'_>) -> Result<T>,
    ) -> Result<T> {
        let before = self.admission();
        let mut mutation = NodeMutation {
            spec: &self.spec,
            operation_id: &mut self.operation_id,
            state: &mut self.state,
            maintenance: &mut self.maintenance,
        };
        let result = mutate(&mut mutation)?;
        if self.admission() != before {
            self.bump_version()?;
        }
        Ok(result)
    }

    pub(crate) fn set_anonymous(&mut self, value: bool) -> Result<bool> {
        if self.anonymous == value {
            return Ok(false);
        }
        self.anonymous = value;
        self.bump_version()?;
        Ok(true)
    }

    fn bump_version(&mut self) -> Result<()> {
        self.version = self.version.checked_add(1).ok_or(Fault::Invalid)?;
        Ok(())
    }
}

#[derive(Serialize)]
pub struct NodeDetails {
    #[serde(flatten)]
    pub node: Node,
    pub admission: Admission,
    pub report: Option<NodeLoadReport>,
    pub cutoff_ready: bool,
}

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
pub struct NodeLoadReport {
    pub created_at: u64,
    pub reported_at: u64,
    pub load: Load,
}

impl NodeDetails {
    fn new(node: Node, report: Option<NodeLoadReport>, now: u64) -> Self {
        let admission = node.admission();
        let cutoff_ready = admission != Admission::Accepting
            && report
                .as_ref()
                .is_some_and(|r| r.confirms(node.version, now) && r.load.resident_shares == 0);
        Self {
            node,
            admission,
            report,
            cutoff_ready,
        }
    }
}

impl NodeLoadReport {
    pub(crate) const fn confirms(&self, policy_version: u64, now: u64) -> bool {
        // Measure confirmation freshness on the authority receipt clock rather than
        // comparing the node's local wall clock against `now`.
        policy_version > 0
            && self.load.applied_policy_version == policy_version
            && now.saturating_sub(self.reported_at) <= 2 * REPORT_INTERVAL_SECONDS
    }
}

pub fn merge_load_report(
    persisted: Option<NodeLoadReport>,
    live: Option<&NodeLoadReport>,
) -> Option<NodeLoadReport> {
    match (persisted, live.cloned()) {
        (Some(persisted), Some(live)) => {
            if live.reported_at >= persisted.reported_at {
                Some(live)
            } else {
                Some(persisted)
            }
        }
        (Some(persisted), None) => Some(persisted),
        (None, Some(live)) => Some(live),
        (None, None) => None,
    }
}

#[derive(Serialize)]
pub struct NodePage {
    pub entries: Vec<NodeDetails>,
    pub next_cursor: Option<String>,
}

#[derive(Clone, Copy, Debug, Deserialize)]
#[serde(
    tag = "field",
    content = "value",
    rename_all = "snake_case",
    deny_unknown_fields
)]
pub enum NodePolicyChange {
    AnonymousSharing(bool),
}

impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Requires a live administrator session and readable node records.
    pub fn nodes(&self, session: &str, after: Option<[u8; 32]>) -> Result<NodePage> {
        self.require_admin(session)?;
        self.node_page(after)
    }

    pub(crate) fn node_page(&self, after: Option<[u8; 32]>) -> Result<NodePage> {
        let after_node = after.map(|id| key("node", &id));
        let mut rows =
            self.store
                .scan_after("node/", after_node.as_deref(), Some(NODE_PAGE_SIZE + 1))?;
        let has_more = rows.len() > NODE_PAGE_SIZE;
        rows.truncate(NODE_PAGE_SIZE);

        // Every load belongs to an enrolled node. The shared key order and cursor
        // keep the selected nodes' load records in one bounded scan.
        let after_load = after.map(|id| key("node-load", &id));
        let loads: BTreeMap<_, _> = self
            .store
            .scan_after("node-load/", after_load.as_deref(), Some(rows.len()))?
            .into_iter()
            .collect();
        let entries = rows
            .into_iter()
            .map(|(_, value)| {
                let node: Node = serde_json::from_str(&value)?;
                let persisted = loads
                    .get(&key("node-load", &node.key))
                    .map(|value| serde_json::from_str::<NodeLoadReport>(value))
                    .transpose()?;
                let report = merge_load_report(persisted, self.live_loads.get(&node.key));
                Ok(NodeDetails::new(node, report, self.environment.now()))
            })
            .collect::<Result<Vec<_>>>()?;
        let next_cursor = entries
            .last()
            .filter(|_| has_more)
            .map(|entry| URL_SAFE_NO_PAD.encode(entry.node.key));
        tracing::debug!(
            event = "authority_nodes_listed",
            continued = after.is_some(),
            count = entries.len(),
            has_more
        );
        Ok(NodePage {
            entries,
            next_cursor,
        })
    }

    /// # Errors
    /// Requires a live administrator session and an existing node.
    pub fn node_by_key(&self, session: &str, id: &[u8; 32]) -> Result<NodeDetails> {
        self.require_admin(session)?;
        let record = key("node", id);
        let node = load(&self.store, &record)?;
        tracing::debug!(event = "authority_node_read", node_key = %record, found = node.is_some());
        self.node_details(node.ok_or(Fault::NodeNotFound)?)
    }

    pub(crate) fn node_details(&self, node: Node) -> Result<NodeDetails> {
        let persisted = load::<NodeLoadReport>(&self.store, &key("node-load", &node.key))?;
        let report = merge_load_report(persisted, self.live_loads.get(&node.key));
        Ok(NodeDetails::new(node, report, self.environment.now()))
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
        match change {
            NodePolicyChange::AnonymousSharing(value) => {
                if !node.set_anonymous(value)? {
                    return Ok(node);
                }
            }
        }
        save(&mut self.store, &record, &node)?;
        tracing::info!(
            event = "node_policy_saved",
            node = record,
            ?change,
            version = node.version
        );
        Ok(node)
    }
}
