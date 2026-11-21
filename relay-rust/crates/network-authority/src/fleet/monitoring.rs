use super::{
    operations::Admission,
    topology::{MAX_NODES, Role, Spec, public_key},
    vantage::{Agent, FRESH_SECONDS, Proof, SILENT_SECONDS},
};
use crate::{
    Authority, Environment, Fault, Node, Store, Zone,
    store::{key, load, save},
};
use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

pub const CYCLE_SECONDS: u64 = 5 * 60;
// Exceeds 2 * FRESH_SECONDS so any previously accepted signed report is already
// outside the freshness window once the next cycle becomes eligible.
pub const MIN_CYCLE_SECONDS: u64 = 4 * 60;
const _: () = assert!(
    MIN_CYCLE_SECONDS > 2 * FRESH_SECONDS
        && MIN_CYCLE_SECONDS < CYCLE_SECONDS
        && CYCLE_SECONDS < SILENT_SECONDS
);
const ASSIGN_DOMAIN: &[u8] = b"windshare/fleet-targets/v1\0";
pub(super) const REPORT_DOMAIN: &[u8] = b"windshare/fleet-cycle/v1\0";
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Request {
    at: u64,
}
#[derive(Serialize)]
pub struct Targets {
    pub zone: Zone,
    pub nodes: Vec<Target>,
}
#[derive(Serialize)]
pub struct Target {
    pub operation_id: String,
    pub spec: Spec,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Observation {
    pub operation_id: String,
    pub tcp: bool,
    pub tls_expires: Option<u64>,
    pub https: bool,
    pub relay: Option<bool>,
    pub stun: Option<bool>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Report {
    at: u64,
    mirror: Option<String>,
    nodes: BTreeMap<String, Observation>,
}
#[derive(Deserialize, Serialize)]
pub struct Cycle {
    pub id: String,
    pub zone: Zone,
    pub at: u64,
    pub mirror: Option<String>,
    pub nodes: BTreeMap<String, Observation>,
}
fn record(agent: &Agent) -> String {
    format!("fleet-cycle/{}", agent.key)
}
pub(super) fn latest(store: &impl Store, agent: &Agent) -> Result<Option<Cycle>> {
    load(store, &record(agent))
}

/// # Errors
/// Propagates storage and monitoring cycle decoding errors.
pub fn reports(store: &impl Store, agents: &[Agent]) -> Result<Vec<Cycle>> {
    let mut result = Vec::new();
    for agent in agents {
        if let Some(cycle) = latest(store, agent)?
            && cycle.zone == agent.zone
        {
            result.push(cycle);
        }
    }
    Ok(result)
}

impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Accepts only fresh requests from dedicated vantage identities.
    pub fn fleet_targets(&self, agents: &[Agent], proof: &Proof) -> Result<Targets> {
        let (agent, request) = proof.open::<Request>(agents, ASSIGN_DOMAIN)?;
        let now = self.environment.now();
        ensure!(
            now.abs_diff(request.at) <= FRESH_SECONDS
                && self
                    .store
                    .get(&key("node", &public_key(&agent.key)?))?
                    .is_none(),
            Fault::Invalid
        );
        let mut nodes = Vec::new();
        for (_, raw) in self.store.scan("node/", None)? {
            // Only listed nodes participate in periodic fleet monitoring; staging drafts
            // have dedicated enrollment verification and disabled nodes are out of service.
            let node: Node = serde_json::from_str(&raw)?;
            if node.status().is_some() {
                nodes.push(Target {
                    operation_id: node.operation_id,
                    spec: node.spec,
                });
            }
        }
        Ok(Targets {
            zone: agent.zone,
            nodes,
        })
    }

    /// # Errors
    /// Rejects forged, oversized, stale or too-frequent reports.
    pub fn record_cycle(&mut self, agents: &[Agent], proof: &Proof) -> Result<()> {
        let (agent, report) = proof.open::<Report>(agents, REPORT_DOMAIN)?;
        let now = self.environment.now();
        ensure!(
            now.abs_diff(report.at) <= FRESH_SECONDS
                && report.nodes.len() <= MAX_NODES
                && report.mirror.as_ref().is_none_or(|m| public_key(m).is_ok())
                && self
                    .store
                    .get(&key("node", &public_key(&agent.key)?))?
                    .is_none(),
            Fault::Invalid
        );
        if let Some(cycle) = latest(&self.store, agent)?
            && now.saturating_sub(cycle.at) < MIN_CYCLE_SECONDS
        {
            tracing::warn!(
                event = "fleet_cycle_rejected",
                vantage = %agent.id,
                reason = "too_frequent",
                elapsed = now.saturating_sub(cycle.at),
                min_seconds = MIN_CYCLE_SECONDS
            );
            return Err(Fault::Invalid.into());
        }
        let mut nodes = BTreeMap::new();
        for (id, observation) in report.nodes {
            let Some(node) = load::<Node>(&self.store, &key("node", &public_key(&id)?))? else {
                continue;
            };
            // Ignore non-listed or superseded operations so concurrent disables and
            // staging drafts do not surface stale probe rows in the console.
            if node.status().is_none() || node.operation_id != observation.operation_id {
                continue;
            }
            for (role, verdict) in [
                (Role::Relay, observation.relay),
                (Role::Stun, observation.stun),
            ] {
                ensure!(
                    node.spec.roles.contains(&role) == verdict.is_some(),
                    Fault::Invalid
                );
            }
            tracing::info!(
                event = "fleet_probe",
                operation_id = %observation.operation_id,
                node_id = %id,
                vantage = %agent.id,
                at = now,
                relay = ?observation.relay,
                stun = ?observation.stun,
                tls_expires = ?observation.tls_expires,
            );
            // Intentionally draining or maintenance-held nodes advertise relay_available=false;
            // only warn on relay unavailability when the node is expected to accept traffic.
            let relay_failed =
                node.admission() == Admission::Accepting && observation.relay == Some(false);
            if !observation.tcp
                || !observation.https
                || relay_failed
                || observation.stun == Some(false)
            {
                tracing::warn!(
                    event = "fleet_probe_failed",
                    operation_id = %observation.operation_id,
                    node_id = %id,
                    vantage = %agent.id,
                    at = now,
                    tcp = observation.tcp,
                    https = observation.https,
                    relay = ?observation.relay,
                    stun = ?observation.stun,
                    tls_expires = ?observation.tls_expires,
                );
            }
            nodes.insert(id, observation);
        }
        save(
            &mut self.store,
            &record(agent),
            &Cycle {
                id: agent.id.clone(),
                zone: agent.zone,
                at: now,
                mirror: report.mirror,
                nodes,
            },
        )
    }
}
