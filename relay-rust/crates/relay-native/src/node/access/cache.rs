use super::Service;
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use windshare_relay_access::{
    AccountId, Issuer,
    entitlement::{self, Entitlement},
    restrictions::Restrictions,
    trust::{self, Generation, TrustStore},
    usage::{NodePolicy, Reply, SignedReply},
};

#[derive(Clone, Default, Serialize, Deserialize)]
pub struct Cache {
    pub generations: Vec<Generation>,
    pub entitlements: Vec<(Issuer, AccountId, Entitlement)>,
    pub restrictions: Vec<(Issuer, Restrictions)>,
    pub policies: Vec<(Issuer, NodePolicy)>,
}
impl Cache {
    pub fn merge(&mut self, next: Self) -> Result<()> {
        for generation in next.generations {
            if let Some(previous) = self
                .generations
                .iter_mut()
                .find(|entry| entry.issuer == generation.issuer)
            {
                if generation.number < previous.number {
                    continue;
                }
                ensure!(
                    generation.number != previous.number || generation == *previous,
                    "issuer equivocation"
                );
                *previous = generation;
            } else {
                self.generations.push(generation);
            }
        }
        for (issuer, account, value) in next.entitlements {
            if let Some((_, _, previous)) = self
                .entitlements
                .iter_mut()
                .find(|(i, a, _)| *i == issuer && *a == account)
            {
                let mut cached = Some(*previous);
                if entitlement::advance(&mut cached, value) {
                    *previous = value;
                }
            } else {
                self.entitlements.push((issuer, account, value));
            }
        }
        for (issuer, value) in next.restrictions {
            if let Some((_, previous)) = self.restrictions.iter_mut().find(|(i, _)| *i == issuer) {
                previous.advance(value);
            } else {
                self.restrictions.push((issuer, value));
            }
        }
        for (issuer, policy) in next.policies {
            if let Some((_, previous)) = self.policies.iter_mut().find(|(i, _)| *i == issuer) {
                if policy.version > previous.version {
                    *previous = policy;
                }
            } else {
                self.policies.push((issuer, policy));
            }
        }
        Ok(())
    }
}
impl Service {
    pub fn apply_cache(&self, cache: Cache) -> Result<()> {
        {
            let mut state = self.state.lock().expect("node access state");
            for generation in cache.generations {
                let current = state
                    .trusted
                    .generations()
                    .into_iter()
                    .find(|entry| entry.issuer == generation.issuer);
                if current.is_none_or(|entry| generation.number >= entry.number) {
                    state
                        .trusted
                        .remember(generation)
                        .map_err(|error| anyhow!("{error:?}"))?;
                }
            }
            for (issuer, policy) in cache.policies {
                if state
                    .node_policies
                    .get(&issuer)
                    .is_none_or(|old| policy.version > old.version)
                {
                    state.node_policies.insert(issuer, policy);
                }
            }
        }
        for (issuer, account, value) in cache.entitlements {
            self.refresh(issuer, account, value);
        }
        for (issuer, restrictions) in cache.restrictions {
            self.restrict(issuer, restrictions);
        }
        self.changes.send_modify(|version| *version += 1);
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
        let mut trusted: TrustStore = state.trusted.clone();
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
