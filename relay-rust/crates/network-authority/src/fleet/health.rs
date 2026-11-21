use super::{
    Listing, ListingState,
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
use std::collections::BTreeSet;

#[derive(Clone, Copy, PartialEq, Eq)]
enum Verdict {
    Withdraw,
    Recover,
    Observe,
}

fn has_quorum(listing: &Listing, reporting: &[Cycle]) -> bool {
    // An overseas path cannot speak for the node's own zone. A lone reporter
    // can raise an alert, but cannot change traffic or clear an override.
    reporting.len() >= 2 && reporting.iter().any(|v| v.zone == listing.spec.zone)
}

fn consensus(listing: &Listing, role: Role, reporting: &[Cycle]) -> Verdict {
    if !has_quorum(listing, reporting) {
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

fn corroborated_failure(listing: &Listing, role: Role, reporting: &[Cycle]) -> bool {
    has_quorum(listing, reporting) && reporting.iter().all(|c| sample(c, listing, role) < 0)
}

fn burst_counts(nodes: &[Node], reporting: &[Cycle]) -> (usize, usize) {
    let mut serving = 0;
    let mut failing = 0;
    // Measure fleet-wide fault bursts strictly against currently serving active
    // roles so single-vantage jitter, already-withdrawn roles, draining nodes,
    // and unproven canaries cannot blind automatic failover on small fleets.
    for listing in nodes.iter().filter_map(|n| n.listing.as_ref()) {
        let ListingState::Listed {
            status: Status::Active,
            withdrawn,
        } = &listing.state
        else {
            continue;
        };
        if listing.maintenance.is_some() {
            continue;
        }
        let mut roles = listing
            .spec
            .roles
            .iter()
            .copied()
            .filter(|role| !withdrawn.contains(role))
            .peekable();
        if roles.peek().is_none() {
            continue;
        }
        serving += 1;
        if roles.any(|role| corroborated_failure(listing, role, reporting)) {
            failing += 1;
        }
    }
    (failing, serving)
}

fn next_withdrawn(
    nodes: &[Node],
    listing: &Listing,
    status: Status,
    withdrawn: &BTreeSet<Role>,
    reporting: &[Cycle],
    burst: bool,
) -> (BTreeSet<Role>, bool) {
    let mut roles = withdrawn.clone();
    let mut suppressed = false;
    for &role in &listing.spec.roles {
        match consensus(listing, role, reporting) {
            Verdict::Recover => {
                roles.remove(&role);
            }
            Verdict::Withdraw
                if status != Status::Active || alternative(nodes, listing, role, reporting) =>
            {
                if burst {
                    suppressed |= !roles.contains(&role);
                } else {
                    roles.insert(role);
                }
            }
            _ => {}
        }
    }
    (roles, suppressed)
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
            let ListingState::Listed {
                status: Status::Active,
                withdrawn,
            } = &l.state
            else {
                return false;
            };
            l.spec.node_key != subject.spec.node_key
                && l.spec.zone == subject.spec.zone
                && l.spec.roles.contains(&role)
                && !withdrawn.contains(&role)
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
        let (failing, serving) = burst_counts(&nodes, &reporting);
        let burst = failing >= 2 && failing * 2 >= serving;
        let operation = format!("health-{}", now / super::monitoring::CYCLE_SECONDS);
        let mut suppressed = false;
        for index in 0..nodes.len() {
            let Some(listing) = nodes[index].listing.as_ref() else {
                continue;
            };
            let ListingState::Listed { status, withdrawn } = &listing.state else {
                continue;
            };
            if listing.maintenance.is_some() {
                continue;
            }
            let (roles, blocked) =
                next_withdrawn(&nodes, listing, *status, withdrawn, &reporting, burst);
            suppressed |= blocked;
            if roles != *withdrawn {
                tracing::warn!(event="fleet_health_override", operation_id=%operation, node_id=%listing.spec.node_key, before=?withdrawn, after=?roles);
                let node = &mut nodes[index];
                let Some(ListingState::Listed { withdrawn, .. }) =
                    node.listing.as_mut().map(|l| &mut l.state)
                else {
                    return Err(crate::Fault::Invalid.into());
                };
                *withdrawn = roles;
                node.apply_listing()?;
                save(&mut self.store, &key("node", &node.key), node)?;
            }
        }
        if suppressed {
            tracing::warn!(
                event = "fleet_health_burst_suppressed",
                operation_id = %operation,
                failing,
                serving,
            );
        }
        head.project(&mut self.store, signer, now, &operation)
    }
}
