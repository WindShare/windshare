use super::{AuthorityState, Service, control::Authorities};
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Serialize};
use std::collections::BTreeSet;
use windshare_relay_access::{Credential, Issuer, trust::Generation};

const DEFAULT_PRINCIPAL_SHARES: usize = 16;
const DEFAULT_OWNER_RESERVE: usize = 32;
pub(super) const AUTHENTICATED_CAPACITY_DIVISOR: usize = 8;

#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct TrustedIssuer {
    pub root: String,
    pub certificate: String,
}

#[derive(Clone, Debug, Deserialize, Serialize, Eq, PartialEq)]
#[serde(default, deny_unknown_fields)]
pub struct Policy {
    pub anonymous: bool,
    pub shares_per_principal: usize,
    pub owner_reserved_shares: usize,
    pub trusted_issuers: Vec<TrustedIssuer>,
    pub credentials: Vec<Credential>,
    pub owner_principals: Vec<[u8; 16]>,
}

impl Default for Policy {
    fn default() -> Self {
        Self {
            anonymous: true,
            shares_per_principal: DEFAULT_PRINCIPAL_SHARES,
            owner_reserved_shares: DEFAULT_OWNER_RESERVE,
            trusted_issuers: Vec::new(),
            credentials: Vec::new(),
            owner_principals: Vec::new(),
        }
    }
}

impl Policy {
    pub(in crate::node) fn generations(&self) -> Result<Vec<Generation>> {
        ensure!(
            self.shares_per_principal > 0
                && self.owner_reserved_shares < crate::config::MAX_RESIDENT_SHARES,
            "invalid resident admission limits"
        );
        let mut roots = BTreeSet::new();
        self.trusted_issuers
            .iter()
            .map(|issuer| {
                let root = URL_SAFE_NO_PAD
                    .decode(&issuer.root)?
                    .try_into()
                    .map_err(|_| anyhow!("issuer root must be an Ed25519 public key"))?;
                ensure!(roots.insert(root), "duplicate trusted issuer");
                Generation::verify(&URL_SAFE_NO_PAD.decode(&issuer.certificate)?, root)
                    .map_err(|error| anyhow!("issuer certificate: {error:?}"))
            })
            .collect()
    }
}

pub(in crate::node) struct PreparedPolicy {
    policy: Policy,
    authorities: Authorities,
    active_issuers: BTreeSet<Issuer>,
}

impl Service {
    pub(in crate::node) fn policy_matches(&self, policy: &Policy) -> bool {
        self.state.lock().expect("node access state").policy == *policy
    }

    pub(in crate::node) fn prepare_policy(
        &self,
        policy: Policy,
        records: Vec<AuthorityState>,
    ) -> Result<PreparedPolicy> {
        let mut authorities = self
            .state
            .lock()
            .map_err(|_| anyhow!("node access state poisoned"))?
            .authorities
            .clone();
        let mut active_issuers = BTreeSet::new();
        for record in records {
            active_issuers.insert(record.generation.issuer);
            authorities.apply(record)?;
        }
        Ok(PreparedPolicy {
            policy,
            authorities,
            active_issuers,
        })
    }

    pub(in crate::node) fn publish_policy(
        &self,
        controller: Option<Issuer>,
        prepared: Option<PreparedPolicy>,
    ) -> bool {
        let mut state = self.state.lock().expect("node access state");
        let controller_changed = state.controller != controller;
        state.controller = controller;

        if let Some(prepared) = prepared {
            state.authorities = prepared.authorities;
            state.active_issuers = prepared.active_issuers;
            state.owners.clone_from(&prepared.policy.owner_principals);
            state.policy = prepared.policy;
            state.rebuild_credentials();
        } else {
            return controller_changed;
        }
        drop(state);
        true
    }

    pub(in crate::node) fn notify_policy_changed(&self) {
        self.changes.send_modify(|version| *version += 1);
    }

    #[cfg(test)]
    pub fn configure(&self, policy: Policy) -> Result<()> {
        let generations = policy.generations()?;
        if !self.policy_matches(&policy) {
            let prepared =
                self.prepare_policy(policy, generations.into_iter().map(Into::into).collect())?;
            let controller = self.state.lock().expect("node access state").controller;
            self.publish_policy(controller, Some(prepared));
            self.notify_policy_changed();
        }
        Ok(())
    }
}
