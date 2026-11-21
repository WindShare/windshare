use super::{
    topology::{MAX_NODES, Role, Spec, public_key},
    vantage::{Agent, FRESH_SECONDS, Proof},
};
use crate::{
    Authority, Environment, Fault, Node, Store, Zone,
    store::{key, load, save},
};
use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize};
use std::collections::BTreeMap;

pub const CYCLE_SECONDS: u64 = 5 * 60;
const ASSIGN_DOMAIN: &[u8] = b"windshare/fleet-targets/v1\0";
pub(super) const REPORT_DOMAIN: &[u8] = b"windshare/fleet-cycle/v1\0";
pub(super) const SUSTAINED: i8 = 3;
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Request {
    at: u64,
}
#[derive(Serialize)]
pub struct Targets {
    pub zone: Zone,
    pub cycle: u64,
    pub nodes: Vec<Target>,
    pub origins: [String; super::vantage::ORIGIN_COUNT],
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
    cycle: u64,
    mirror: Option<String>,
    nodes: BTreeMap<String, Observation>,
}
#[derive(Deserialize, Serialize)]
pub struct Sample {
    #[serde(flatten)]
    pub observation: Observation,
    pub streak: BTreeMap<Role, i8>,
}
#[derive(Deserialize, Serialize)]
pub struct Cycle {
    pub id: String,
    pub zone: Zone,
    pub at: u64,
    pub cycle: u64,
    pub mirror: Option<String>,
    pub nodes: BTreeMap<String, Sample>,
}
fn record(agent: &Agent) -> String {
    format!("fleet-cycle/{}", agent.key)
}
pub(super) fn latest(store: &impl Store, agent: &Agent) -> Result<Option<Cycle>> {
    load(store, &record(agent))
}

pub(crate) fn reports(store: &impl Store, agents: &[Agent]) -> Result<Vec<Cycle>> {
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

pub(super) fn reset_evidence(store: &mut impl Store, node: &str, role: Role) -> Result<()> {
    for (key, raw) in store.scan("fleet-cycle/", None)? {
        let mut cycle: Cycle = serde_json::from_str(&raw)?;
        if let Some(sample) = cycle.nodes.get_mut(node)
            && sample.streak.remove(&role).is_some()
        {
            save(store, &key, &cycle)?;
        }
    }
    Ok(())
}

fn streak(previous: i8, pass: bool) -> i8 {
    if pass {
        previous.max(0).saturating_add(1)
    } else {
        previous.min(0).saturating_sub(1)
    }
}

impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Accepts only fresh requests from dedicated vantage identities.
    pub fn fleet_targets(
        &self,
        agents: &[Agent],
        proof: &Proof,
        origins: [String; super::vantage::ORIGIN_COUNT],
    ) -> Result<Targets> {
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
            if let Some(listing) = serde_json::from_str::<Node>(&raw)?.listing
                && listing.status().is_some()
            {
                nodes.push(Target {
                    operation_id: listing.operation_id,
                    spec: listing.spec,
                });
            }
        }
        Ok(Targets {
            zone: agent.zone,
            cycle: now / CYCLE_SECONDS,
            nodes,
            origins,
        })
    }

    /// # Errors
    /// Rejects forged, oversized, stale or duplicate cycles. Streaks count distinct consecutive cycles.
    pub fn record_cycle(&mut self, agents: &[Agent], proof: &Proof) -> Result<()> {
        let (agent, report) = proof.open::<Report>(agents, REPORT_DOMAIN)?;
        let now = self.environment.now();
        let current = now / CYCLE_SECONDS;
        ensure!(
            now.abs_diff(report.at) <= FRESH_SECONDS
                && (current.saturating_sub(1)..=current).contains(&report.cycle)
                && report.nodes.len() <= MAX_NODES
                && report.mirror.as_ref().is_none_or(|m| public_key(m).is_ok())
                && self
                    .store
                    .get(&key("node", &public_key(&agent.key)?))?
                    .is_none(),
            Fault::Invalid
        );
        let previous = latest(&self.store, agent)?;
        ensure!(
            previous.as_ref().is_none_or(|c| c.cycle < report.cycle),
            Fault::Invalid
        );
        let previous = previous.filter(|c| c.cycle + 1 == report.cycle && c.zone == agent.zone);
        let mut nodes = BTreeMap::new();
        for (id, observation) in report.nodes {
            let Some(node) = load::<Node>(&self.store, &key("node", &public_key(&id)?))? else {
                continue;
            };
            let Some(listing) = node.listing else {
                continue;
            };
            // Ignore non-listed nodes rather than rejecting the whole cycle so concurrent
            // disables stay harmless and staging drafts cannot accumulate failure streaks.
            if listing.status().is_none() || listing.operation_id != observation.operation_id {
                continue;
            }
            let old = previous
                .as_ref()
                .and_then(|c| c.nodes.get(&id))
                .filter(|s| s.observation.operation_id == observation.operation_id);
            let mut counts = BTreeMap::new();
            for (role, verdict) in [
                (Role::Relay, observation.relay),
                (Role::Stun, observation.stun),
            ] {
                ensure!(
                    listing.spec.roles.contains(&role) == verdict.is_some(),
                    Fault::Invalid
                );
                if let Some(pass) = verdict {
                    let count = streak(
                        old.and_then(|s| s.streak.get(&role)).copied().unwrap_or(0),
                        pass,
                    );
                    counts.insert(role, count);
                }
            }
            tracing::info!(event="fleet_probe", operation_id=%observation.operation_id, node_id=%id, vantage=%agent.id, cycle=report.cycle, relay=?observation.relay,stun=?observation.stun,tls_expires=?observation.tls_expires);
            nodes.insert(
                id,
                Sample {
                    observation,
                    streak: counts,
                },
            );
        }
        save(
            &mut self.store,
            &record(agent),
            &Cycle {
                id: agent.id.clone(),
                zone: agent.zone,
                cycle: report.cycle,
                at: now,
                mirror: report.mirror,
                nodes,
            },
        )
    }
}
