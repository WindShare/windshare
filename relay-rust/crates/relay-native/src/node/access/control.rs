use super::Service;
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use std::collections::BTreeMap;
use windshare_relay_access::{
    Issuer,
    restrictions::Restrictions,
    trust::{self, Generation, TrustStore},
    usage::{NodePolicy, Reply, SignedReply},
};

use super::quota::{AccountKey, Quota};

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct AuthorityState {
    pub generation: Generation,
    pub policy: Option<NodePolicy>,
}

impl From<Generation> for AuthorityState {
    fn from(generation: Generation) -> Self {
        Self {
            generation,
            policy: None,
        }
    }
}

impl AuthorityState {
    pub(crate) fn merge(&mut self, next: Self) -> Result<()> {
        let generation = next.generation;
        ensure!(
            generation.number != self.generation.number || generation == self.generation,
            "issuer equivocation"
        );
        if generation.number > self.generation.number {
            self.generation = generation;
        }
        if let Some(policy) = next.policy
            && self
                .policy
                .is_none_or(|previous| policy.version > previous.version)
        {
            self.policy = Some(policy);
        }
        Ok(())
    }
}

pub struct AccessUpdate {
    pub authority: AuthorityState,
    pub quotas: Vec<(AccountKey, Quota)>,
    pub quota_day_floor: u64,
}

pub struct AccessSnapshot {
    pub authorities: Vec<AuthorityState>,
    pub restrictions: Vec<(Issuer, Restrictions)>,
    pub quotas: Vec<(AccountKey, Quota)>,
    pub quota_day_floor: u64,
}

#[derive(Clone, Default)]
pub(super) struct Authorities {
    pub(super) trusted: TrustStore,
    pub(super) policies: BTreeMap<Issuer, NodePolicy>,
}

impl Authorities {
    pub(super) fn apply(&mut self, authority: AuthorityState) -> Result<bool> {
        let generation = authority.generation;
        let issuer = generation.issuer;
        let previous = self
            .trusted
            .generations()
            .into_iter()
            .find(|entry| entry.issuer == issuer);
        if previous.is_none_or(|entry| generation.number >= entry.number) {
            self.trusted
                .remember(generation)
                .map_err(|error| anyhow!("{error:?}"))?;
        }
        if let Some(policy) = authority.policy
            && self
                .policies
                .get(&issuer)
                .is_none_or(|old| policy.version > old.version)
        {
            self.policies.insert(issuer, policy);
            return Ok(true);
        }
        Ok(previous.is_none_or(|entry| generation.number > entry.number))
    }
}

impl Service {
    pub(crate) fn restore_access(&self, snapshot: AccessSnapshot, now: u64) -> Result<()> {
        let mut state = self.state.lock().expect("node access state");
        for authority in snapshot.authorities {
            state.authorities.apply(authority)?;
        }
        for (issuer, restrictions) in snapshot.restrictions {
            state
                .restrictions
                .entry(issuer)
                .or_default()
                .advance(restrictions);
        }
        state.quotas.retire_before(snapshot.quota_day_floor, now);
        for (key, quota) in snapshot.quotas {
            state.quotas.advance(key, quota, now);
        }
        drop(state);
        self.changes.send_modify(|version| *version += 1);
        Ok(())
    }

    pub(crate) fn apply_authority(&self, authority: AuthorityState) -> Result<()> {
        let changed = self
            .state
            .lock()
            .expect("node access state")
            .authorities
            .apply(authority)?;
        if changed {
            self.changes.send_modify(|version| *version += 1);
        }
        Ok(())
    }

    pub(crate) fn apply_access_update(&self, update: AccessUpdate, now: u64) -> Result<()> {
        let mut state = self.state.lock().expect("node access state");
        let changed = state.authorities.apply(update.authority)?;
        state.quotas.retire_before(update.quota_day_floor, now);
        let mut advanced = 0;
        for (key, quota) in update.quotas {
            advanced += usize::from(state.quotas.advance(key, quota, now));
        }
        drop(state);
        if changed || advanced != 0 {
            self.changes.send_modify(|version| *version += 1);
            tracing::debug!(
                event = "node_access_updated",
                issuer = ?update.authority.generation.issuer,
                generation = update.authority.generation.number,
                advanced,
            );
        }
        Ok(())
    }

    pub fn verify_reply<T: DeserializeOwned>(
        &self,
        signed: SignedReply,
        issuer: Issuer,
        node: [u8; 32],
        request: [u8; 16],
        now: u64,
        domain: &[u8],
    ) -> Result<(Generation, Reply<T>)> {
        let state = self.state.lock().expect("node access state");
        ensure!(state.active_issuers.contains(&issuer), "inactive authority");
        let mut trusted: TrustStore = state.authorities.trusted.clone();
        drop(state);
        let generation = Generation::verify(&URL_SAFE_NO_PAD.decode(signed.certificate)?, issuer)
            .map_err(|error| anyhow!("authority certificate: {error:?}"))?;
        trusted
            .accept(generation, now)
            .map_err(|error| anyhow!("authority generation: {error:?}"))?;
        let bytes = URL_SAFE_NO_PAD.decode(signed.body)?;
        let payload = trust::verify(domain, &bytes, &generation.key)
            .map_err(|error| anyhow!("authority signature: {error:?}"))?;
        let reply: Reply<T> = serde_json::from_slice(payload)?;
        ensure!(
            reply.node == node
                && reply.request == request
                && reply.issuer == issuer
                && reply.generation == generation.number,
            "authority response binding"
        );
        ensure!(
            reply.issued_at <= now.saturating_add(trust::CLOCK_SKEW),
            "authority response time"
        );
        Ok((generation, reply))
    }
}
