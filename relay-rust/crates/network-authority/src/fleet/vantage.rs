use super::{
    EVIDENCE_SECONDS, ListingState, Verification,
    signature::verify_bytes,
    topology::{Role, Spec, public_key},
};
use crate::{
    Authority, Environment, Fault, Node, Store, Zone,
    store::{key, load, save},
};
use anyhow::{Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD as B64};
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use windshare_relay_access::{
    entitlement::{DAY_SECONDS, Entitlement},
    probe,
    ticket::Ticket,
};

const POLL_DOMAIN: &[u8] = b"windshare/vantage-poll/v1\0";
const RESULT_DOMAIN: &[u8] = b"windshare/vantage-result/v1\0";
const FRESH_SECONDS: u64 = 90;
const PROBE_ALLOWANCE_BYTES: u64 = 4 * 1024;
const RETRY_SECONDS: u64 = 60;
const MAX_RETRY_SHIFT: u8 = 5; // 32 minutes between retries for an unattended broken draft.
pub const SILENT_SECONDS: u64 = 5 * 60;
pub const ORIGIN_COUNT: usize = 3;

#[derive(Clone, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Agent {
    pub id: String,
    pub key: String,
    pub zone: Zone,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Proof {
    key: String,
    payload: String,
    signature: String,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Poll {
    at: u64,
    shares: [[u8; 12]; ORIGIN_COUNT],
    mirror: Option<String>,
}
#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ProbeReport {
    at: u64,
    node_key: String,
    lease: String,
    outcome: ProbeOutcome,
}
#[derive(Debug, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
enum ProbeOutcome {
    Passed,
    Failed { reason: String },
    Released,
}
#[derive(Deserialize, Serialize)]
struct Heartbeat {
    zone: Zone,
    at: u64,
    mirror: Option<String>,
}
#[derive(Serialize)]
pub struct Work {
    pub operation_id: String,
    pub lease: String,
    pub spec: Spec,
    pub origins: [String; ORIGIN_COUNT],
    pub tickets: Vec<String>,
}

impl Proof {
    fn open<'a, T: DeserializeOwned>(
        &self,
        agents: &'a [Agent],
        domain: &[u8],
    ) -> Result<(&'a Agent, T)> {
        let agent = agents
            .iter()
            .find(|a| a.key == self.key)
            .ok_or_else(|| Fault::Denied {
                reason: "unknown_vantage".into(),
            })?;
        ensure!(self.payload.len() <= 4096, Fault::Invalid);
        let data = B64.decode(&self.payload).map_err(|_| Fault::Invalid)?;
        verify_bytes(&agent.key, domain, &data, &self.signature).map_err(|_| Fault::Denied {
            reason: "vantage_signature".into(),
        })?;
        Ok((
            agent,
            serde_json::from_slice(&data).map_err(|_| Fault::Invalid)?,
        ))
    }
}
/// # Errors
/// Rejects malformed entries, duplicate names and reused signing keys.
pub fn validate_agents(agents: &[Agent]) -> Result<()> {
    let mut ids = std::collections::BTreeSet::new();
    let mut keys = std::collections::BTreeSet::new();
    for (index, agent) in agents.iter().enumerate() {
        let bytes = public_key(&agent.key)
            .map_err(|_| anyhow::anyhow!("invalid VANTAGE_KEYS[{index}].key"))?;
        let key = ed25519_dalek::VerifyingKey::from_bytes(&bytes)?;
        ensure!(!key.is_weak(), "weak VANTAGE_KEYS[{index}].key");
        ensure!(
            !agent.id.is_empty()
                && agent.id.len() <= 64
                && agent
                    .id
                    .bytes()
                    .all(|b| b.is_ascii_alphanumeric() || b == b'-'),
            "invalid VANTAGE_KEYS[{index}].id"
        );
        ensure!(ids.insert(&agent.id), "duplicate VANTAGE_KEYS[{index}].id");
        ensure!(
            keys.insert(&agent.key),
            "duplicate VANTAGE_KEYS[{index}].key"
        );
    }
    Ok(())
}

/// # Errors
/// Propagates storage errors and malformed signed-directory data.
pub fn mainland_current(
    store: &impl Store,
    agents: &[Agent],
    expected: &str,
    now: u64,
) -> Result<bool> {
    let mut seen = false;
    for agent in agents.iter().filter(|agent| agent.zone == Zone::Cn) {
        let Some(beat) = load::<Heartbeat>(store, &format!("vantage/{}", agent.key))? else {
            continue;
        };
        if beat.zone == agent.zone && now.saturating_sub(beat.at) <= SILENT_SECONDS {
            if beat.mirror.as_deref() != Some(expected) {
                return Ok(false);
            }
            seen = true;
        }
    }
    Ok(seen)
}

/// # Errors
/// Propagates storage and heartbeat decoding errors.
pub fn silent(store: &impl Store, agents: &[Agent], now: u64) -> Result<Vec<String>> {
    let mut missing = Vec::new();
    for agent in agents {
        let beat = load::<Heartbeat>(store, &format!("vantage/{}", agent.key))?;
        if beat.is_none_or(|b| now.saturating_sub(b.at) > SILENT_SECONDS) {
            missing.push(agent.id.clone());
        }
    }
    Ok(missing)
}
impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Rejects unknown identities, invalid signatures, stale polls and unavailable storage.
    pub fn claim(
        &mut self,
        agents: &[Agent],
        proof: &Proof,
        origins: [String; ORIGIN_COUNT],
    ) -> Result<Option<Work>> {
        let (agent, poll) = proof.open::<Poll>(agents, POLL_DOMAIN)?;
        let now = self.environment.now();
        let beat = format!("vantage/{}", agent.key);
        ensure!(
            now.abs_diff(poll.at) <= FRESH_SECONDS
                && poll.shares.iter().all(|s| *s != [0; 12])
                && poll
                    .shares
                    .iter()
                    .collect::<std::collections::BTreeSet<_>>()
                    .len()
                    == ORIGIN_COUNT
                && poll.mirror.as_ref().is_none_or(|s| s.len() == 43)
                && load::<Heartbeat>(&self.store, &beat)?.is_none_or(|b| poll.at > b.at),
            Fault::Invalid
        );
        ensure!(
            self.store
                .get(&key("node", &public_key(&agent.key)?))?
                .is_none(),
            Fault::Invalid
        );
        save(
            &mut self.store,
            &beat,
            &Heartbeat {
                zone: agent.zone,
                at: poll.at,
                mirror: poll.mirror,
            },
        )?;
        let mut selected = None;
        for (_, raw) in self.store.scan("node/", None)? {
            let node: Node = serde_json::from_str(&raw)?;
            let Some(listing) = &node.listing else {
                continue;
            };
            if listing.spec.zone != agent.zone {
                continue;
            }
            let ListingState::Staging { check, .. } = &listing.state else {
                continue;
            };
            let due = match check {
                Verification::Queued => 0,
                Verification::Running { until, .. } => *until,
                Verification::Failed { at, failures, .. } => at.saturating_add(
                    RETRY_SECONDS << failures.saturating_sub(1).min(MAX_RETRY_SHIFT),
                ),
                Verification::Passed { at, .. } => at + EVIDENCE_SECONDS,
            };
            if due <= now && selected.as_ref().is_none_or(|(old, _)| due < *old) {
                selected = Some((due, node));
            }
        }
        let Some((_, mut node)) = selected else {
            return Ok(None);
        };
        let lease = B64.encode(self.environment.random::<16>()?);
        let listing = node.listing.as_mut().ok_or(Fault::Invalid)?;
        let until = (now + probe::LIFETIME).min(self.signing.generation.expires_at);
        let tickets = if listing.spec.roles.contains(&Role::Relay) {
            poll.shares
                .iter()
                .map(|share| self.probe_ticket(&node.key, share, now, until))
                .collect::<Result<_>>()?
        } else {
            Vec::new()
        };
        let ListingState::Staging { check, .. } = &mut listing.state else {
            return Err(Fault::Invalid.into());
        };
        let failures = match check {
            Verification::Failed { failures, .. } | Verification::Running { failures, .. } => {
                *failures
            }
            _ => 0,
        };
        *check = Verification::Running {
            vantage_key: agent.key.clone(),
            lease: lease.clone(),
            until,
            failures,
        };
        let work = Work {
            operation_id: listing.operation_id.clone(),
            lease,
            spec: listing.spec.clone(),
            origins,
            tickets,
        };
        tracing::info!(event="verification_claimed",operation_id=%listing.operation_id,node_id=%listing.spec.node_key,vantage=%agent.id);
        save(&mut self.store, &key("node", &node.key), &node)?;
        Ok(Some(work))
    }
    fn probe_ticket(
        &self,
        node: &[u8; 32],
        share: &[u8; 12],
        now: u64,
        until: u64,
    ) -> Result<String> {
        let generation = self.signing.generation;
        let day = now / DAY_SECONDS;
        let ticket = Ticket {
            issuer: generation.issuer,
            key_id: generation.number,
            account: probe::PROBE_ACCOUNT,
            credential: probe::credential(node, share),
            issued_at: now,
            expires_at: until,
            entitlement: Entitlement {
                version: 1,
                day,
                valid_until: until.min((day + 1) * DAY_SECONDS),
                remaining_bytes: PROBE_ALLOWANCE_BYTES,
            },
        };
        Ok(B64.encode(
            ticket
                .issue(generation, &self.signing.key)
                .map_err(|_| Fault::Invalid)?,
        ))
    }
    /// # Errors
    /// Rejects unauthenticated, expired or superseded leases and storage failures.
    pub fn complete_probe(&mut self, agents: &[Agent], proof: &Proof) -> Result<()> {
        let (agent, result) = proof.open::<ProbeReport>(agents, RESULT_DOMAIN)?;
        let now = self.environment.now();
        ensure!(
            now.abs_diff(result.at) <= FRESH_SECONDS
                && !matches!(&result.outcome, ProbeOutcome::Failed { reason } if reason.is_empty() || reason.len() > 256),
            Fault::Invalid
        );
        let record = key("node", &public_key(&result.node_key)?);
        let mut node: Node = load(&self.store, &record)?.ok_or(Fault::NodeNotFound)?;
        let listing = node.listing.as_mut().ok_or(Fault::Invalid)?;
        let ListingState::Staging { check, .. } = &mut listing.state else {
            return Err(Fault::Invalid.into());
        };
        let Verification::Running {
            vantage_key,
            lease,
            until,
            failures,
        } = check
        else {
            return Err(Fault::Invalid.into());
        };
        ensure!(
            listing.spec.zone == agent.zone
                && *vantage_key == agent.key
                && *lease == result.lease
                && now < *until,
            Fault::Invalid
        );
        tracing::info!(event="verification_completed",operation_id=%listing.operation_id,node_id=%result.node_key,vantage=%agent.id,outcome=?result.outcome);
        let consecutive = failures.saturating_add(1);
        *check = match result.outcome {
            ProbeOutcome::Released => Verification::Queued,
            ProbeOutcome::Passed => Verification::Passed {
                vantage_key: agent.key.clone(),
                at: now,
            },
            ProbeOutcome::Failed { reason } => Verification::Failed {
                vantage_key: agent.key.clone(),
                at: now,
                reason,
                failures: consecutive,
            },
        };
        save(&mut self.store, &record, &node)
    }
}
