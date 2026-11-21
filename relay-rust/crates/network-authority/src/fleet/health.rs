use super::{
    Listing,
    monitoring::{Cycle, SUSTAINED, reports},
    publication::{Publication, Publisher},
    topology::{Role, Status},
    vantage::{Agent, SILENT_SECONDS},
};
use crate::{
    Authority, Environment, Node, Store,
    store::{key, save},
};
use anyhow::Result;

#[derive(Clone, Copy, PartialEq, Eq)]
enum Verdict {
    Withdraw,
    Recover,
    Observe,
}

fn consensus(listing: &Listing, role: Role, reporting: &[Cycle]) -> Verdict {
    // An overseas path cannot speak for the node's own zone. A lone reporter
    // can raise an alert, but cannot change traffic or clear an override.
    if reporting.len() < 2 || !reporting.iter().any(|v| v.zone == listing.spec.zone) {
        return Verdict::Observe;
    }
    let mut failures = true;
    let mut successes = true;
    for cycle in reporting {
        let count = sample(cycle, listing, role);
        failures &= count <= -SUSTAINED;
        successes &= count >= SUSTAINED;
    }
    if failures {
        Verdict::Withdraw
    } else if successes {
        Verdict::Recover
    } else {
        Verdict::Observe
    }
}
fn sample(cycle: &Cycle, listing: &Listing, role: Role) -> i8 {
    cycle
        .nodes
        .get(&listing.spec.node_key)
        .filter(|s| s.observation.operation_id == listing.operation_id)
        .and_then(|s| s.streak.get(&role))
        .copied()
        .unwrap_or(0)
}

fn alternative(nodes: &[Node], subject: &Listing, role: Role, reporting: &[Cycle]) -> bool {
    nodes
        .iter()
        .filter_map(|node| node.listing.as_ref())
        .any(|l| {
            l.spec.node_key != subject.spec.node_key
                && l.spec.zone == subject.spec.zone
                && l.status() == Some(Status::Active)
                && l.spec.roles.contains(&role)
                && !l.withdrawn.contains(&role)
                && (role != Role::Relay || !l.relay_drained())
                && reporting.iter().all(|c| sample(c, l, role) > 0)
        })
}

impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Propagates storage and signing failures so health and publication commit together.
    pub fn evaluate_health(
        &mut self,
        agents: &[Agent],
        signer: &Publisher,
        head: &mut Publication,
    ) -> Result<()> {
        let now = self.environment.now();
        let reporting: Vec<_> = reports(&self.store, agents)?
            .into_iter()
            .filter(|c| now.saturating_sub(c.at) <= SILENT_SECONDS)
            .collect();
        let mut nodes = self
            .store
            .scan("node/", None)?
            .into_iter()
            .map(|(_, raw)| serde_json::from_str::<Node>(&raw))
            .collect::<Result<Vec<_>, _>>()?;
        let eligible: Vec<_> = nodes
            .iter()
            .filter_map(|n| n.listing.as_ref())
            .filter(|l| l.status().is_some() && l.maintenance.is_none())
            .collect();
        let failing = eligible
            .iter()
            .filter(|l| {
                l.spec
                    .roles
                    .iter()
                    .any(|r| reporting.iter().any(|c| sample(c, l, *r) < 0))
            })
            .count();
        let burst = failing >= 2 && failing * 2 >= eligible.len();
        let operation = format!("health-{}", now / super::monitoring::CYCLE_SECONDS);
        for index in 0..nodes.len() {
            let Some(listing) = nodes[index].listing.as_ref() else {
                continue;
            };
            if listing.status().is_none() || listing.maintenance.is_some() {
                continue;
            }
            let mut roles = listing.withdrawn.clone();
            for &role in &listing.spec.roles {
                match consensus(listing, role, &reporting) {
                    Verdict::Recover => {
                        roles.remove(&role);
                    }
                    Verdict::Withdraw
                        if !burst
                            && (listing.status() != Some(Status::Active)
                                || alternative(&nodes, listing, role, &reporting)) =>
                    {
                        roles.insert(role);
                    }
                    _ => {}
                }
            }
            if roles != listing.withdrawn {
                tracing::warn!(event="fleet_health_override", operation_id=%operation, node_id=%listing.spec.node_key, before=?listing.withdrawn, after=?roles);
                let node = &mut nodes[index];
                node.listing
                    .as_mut()
                    .ok_or(crate::Fault::Invalid)?
                    .withdrawn = roles;
                node.apply_listing()?;
                save(&mut self.store, &key("node", &node.key), node)?;
            }
        }
        head.project(&mut self.store, signer, now, &operation)
    }
}
