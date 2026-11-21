use super::{
    Listing, ListingState,
    operations::Admission,
    publication::{self, Publisher},
    topology::{Role, Status as ListingStatus, public_key},
    vantage::{ORIGIN_COUNT, Work},
};
use crate::{
    Authority, Environment, Fault, Node, Store,
    store::{key, load, save},
};
use anyhow::{Result, ensure};
use serde::{Deserialize, Serialize};
use windshare_relay_access::probe;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Operation {
    pub node_key: String,
    pub operation_id: String,
}
#[derive(Serialize)]
pub struct Status {
    pub held: bool,
    pub confirmed: bool,
    pub policy_version: u64,
    pub applied_policy_version: Option<u64>,
    pub resident_shares: Option<u64>,
    pub active_sessions: Option<u64>,
}
fn valid_operation_id(operation_id: &str) -> bool {
    (16..=64).contains(&operation_id.len())
        && operation_id
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'-')
}

fn complete_key(node_key: &str, operation_id: &str) -> String {
    format!("maintenance-complete/{node_key}/{operation_id}")
}

/// Clears a stuck maintenance hold under administrator control, tombstoning the
/// aborted operation ID so late runner requests cannot reclaim it.
pub(super) fn abort(
    store: &mut impl Store,
    listing: &mut Listing,
    operation_id: &str,
    maintenance_id: &str,
) -> Result<()> {
    ensure!(
        valid_operation_id(maintenance_id)
            && !matches!(listing.state, ListingState::Staging { .. }),
        Fault::Invalid
    );
    ensure!(
        listing.maintenance.as_deref() == Some(maintenance_id),
        Fault::Conflict {
            reason: "listing_changed".into()
        }
    );
    listing.maintenance = None;
    if let ListingState::Listed { status, .. } = &mut listing.state {
        // Fail closed after an aborted upgrade: keep the node draining until
        // an operator verifies the host and explicitly promotes it back.
        *status = ListingStatus::Draining;
    }
    save(
        store,
        &complete_key(&listing.spec.node_key, maintenance_id),
        &true,
    )?;
    tracing::warn!(
        event = "maintenance_hold_aborted",
        operation_id = %operation_id,
        maintenance_id = %maintenance_id,
        node_id = %listing.spec.node_key,
        disabled = matches!(listing.state, ListingState::Disabled),
    );
    Ok(())
}

impl Operation {
    fn record(&self) -> Result<String> {
        ensure!(valid_operation_id(&self.operation_id), Fault::Invalid);
        Ok(key("node", &public_key(&self.node_key)?))
    }
}
impl<R: Store, E: Environment> Authority<R, E> {
    fn maintenance_node(&self, cmd: &Operation) -> Result<Node> {
        load(&self.store, &cmd.record()?)?.ok_or_else(|| Fault::NodeNotFound.into())
    }

    /// # Errors
    /// Refuses overlapping maintenance, stale operation IDs, and unlisted nodes.
    pub fn maintenance_hold(
        &mut self,
        cmd: &Operation,
        acquire: bool,
        publisher: &Publisher,
    ) -> Result<Status> {
        let mut node = self.maintenance_node(cmd)?;
        let listing = node.listing.as_mut().ok_or(Fault::Invalid)?;
        let owner = listing.maintenance.as_deref();
        let done = complete_key(&listing.spec.node_key, &cmd.operation_id);
        if acquire {
            ensure!(
                listing.status().is_some() && self.store.get(&done)?.is_none(),
                Fault::Invalid
            );
            if owner == Some(&cmd.operation_id) {
                return self.maintenance_status(cmd);
            }
            for (_, raw) in self.store.scan("node/", None)? {
                let other: Node = serde_json::from_str(&raw)?;
                ensure!(
                    other.listing.is_none_or(|l| l.maintenance.is_none()),
                    Fault::Conflict {
                        reason: "maintenance_in_progress".into()
                    }
                );
            }
            listing.maintenance = Some(cmd.operation_id.clone());
        } else {
            if owner.is_none() {
                // A timed-out acquire can still arrive after its matching release.
                if self.store.get(&done)?.is_none() {
                    save(&mut self.store, &done, &true)?;
                }
                return self.maintenance_status(cmd);
            }
            ensure!(
                owner == Some(&cmd.operation_id),
                Fault::Conflict {
                    reason: "maintenance_owner".into()
                }
            );
            listing.maintenance = None;
            save(&mut self.store, &done, &true)?;
        }
        node.apply_listing()?;
        save(&mut self.store, &cmd.record()?, &node)?;
        tracing::info!(event="maintenance_hold_changed",operation_id=%cmd.operation_id,node_id=%cmd.node_key,held=acquire,admission=?node.admission,version=node.version);
        publication::current(&self.store)?.project(
            &mut self.store,
            publisher,
            self.environment.now(),
            &cmd.operation_id,
        )?;
        self.maintenance_status(cmd)
    }

    /// # Errors
    /// Returns only the requested node's signed drain and session counters.
    pub fn maintenance_status(&self, cmd: &Operation) -> Result<Status> {
        let node = self.maintenance_node(cmd)?;
        let listing = node.listing.as_ref().ok_or(Fault::Invalid)?;
        let admission = node.admission;
        let held = listing.maintenance.as_ref() == Some(&cmd.operation_id);
        let policy_version = node.version;
        let report = self.node_details(node)?.report;
        let confirmed = admission == Admission::Draining
            && report
                .as_ref()
                .is_some_and(|r| r.confirms(policy_version, self.environment.now()));
        let load = report.map(|r| r.load);
        Ok(Status {
            held,
            confirmed,
            policy_version,
            applied_policy_version: load.as_ref().map(|l| l.applied_policy_version),
            resident_shares: load.as_ref().map(|l| l.resident_shares),
            active_sessions: load.map(|l| l.active_sessions),
        })
    }

    /// # Errors
    /// Only the owner of a drain can request tickets for its target and synthetic shares.
    pub fn maintenance_probe(
        &self,
        cmd: &Operation,
        shares: &[[u8; 12]; ORIGIN_COUNT],
        origins: [String; ORIGIN_COUNT],
    ) -> Result<Work> {
        let node = self.maintenance_node(cmd)?;
        let listing = node.listing.ok_or(Fault::Invalid)?;
        ensure!(
            listing.maintenance.as_ref() == Some(&cmd.operation_id)
                && node.admission == Admission::Draining,
            Fault::Invalid
        );
        ensure!(
            shares.iter().all(|id| *id != [0; 12])
                && shares
                    .iter()
                    .collect::<std::collections::BTreeSet<_>>()
                    .len()
                    == ORIGIN_COUNT,
            Fault::Invalid
        );
        let now = self.environment.now();
        let until = (now + probe::LIFETIME).min(self.signing.generation.expires_at);
        let tickets = if listing.spec.roles.contains(&Role::Relay) {
            shares
                .iter()
                .map(|id| self.probe_ticket(&node.key, id, now, until))
                .collect::<Result<_>>()?
        } else {
            Vec::new()
        };
        Ok(Work {
            spec: listing.spec,
            origins,
            tickets,
            operation_id: cmd.operation_id.clone(),
            lease: cmd.operation_id.clone(),
        })
    }
}
