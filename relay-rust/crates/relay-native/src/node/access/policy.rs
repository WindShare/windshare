use super::{AuthorityState, CredentialIndex, DeviceKeys, Service, control::Authorities};
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Serialize};
use std::{
    collections::BTreeSet,
    sync::{Arc, MutexGuard},
};
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
    #[serde(skip, default = "contributing")]
    pub(crate) contributing: bool,
    #[serde(skip, default = "unlimited_connections")]
    pub(crate) public_connections: usize,
    pub shares_per_principal: usize,
    pub owner_reserved_shares: usize,
    pub trusted_issuers: Vec<TrustedIssuer>,
    pub credentials: Vec<Credential>,
    pub owner_principals: Vec<[u8; 16]>,
}

const fn unlimited_connections() -> usize {
    usize::MAX
}
const fn contributing() -> bool {
    true
}

impl Default for Policy {
    fn default() -> Self {
        Self {
            anonymous: true,
            contributing: true,
            public_connections: usize::MAX,
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

struct PolicyState {
    policy: Arc<Policy>,
    authorities: Authorities,
    active_issuers: BTreeSet<Issuer>,
    credentials: Arc<CredentialIndex>,
}

pub(in crate::node) struct PreparedPolicy<'a> {
    next: PolicyState,
    // A console write cannot supersede the device input between preparation and publication.
    _credential_writer: MutexGuard<'a, DeviceKeys>,
}

pub(in crate::node) struct PolicyPublication {
    pub changed: bool,
    // The caller publishes inside the node/governor transaction. Free the old index afterward.
    _retired: Option<PolicyState>,
}

impl Service {
    pub(in crate::node) fn policy_matches(&self, policy: &Policy) -> bool {
        let current = Arc::clone(&self.read().policy);
        current.as_ref() == policy
    }

    pub(in crate::node) fn prepare_policy(
        &self,
        policy: Policy,
        records: Vec<AuthorityState>,
    ) -> Result<PreparedPolicy<'_>> {
        let devices = self.devices.lock().expect("credential writer");
        let (mut authorities, current, previous) = {
            let state = self.read();
            (
                state.authorities.clone(),
                Arc::clone(&state.policy),
                Arc::clone(&state.credentials),
            )
        };
        let mut active_issuers = BTreeSet::new();
        for record in records {
            active_issuers.insert(record.generation.issuer);
            authorities.apply(record)?;
        }
        let credentials = if current.credentials == policy.credentials {
            previous
        } else {
            Arc::new(CredentialIndex::build(
                &policy.credentials,
                &devices,
                &previous,
            ))
        };
        Ok(PreparedPolicy {
            next: PolicyState {
                policy: Arc::new(policy),
                authorities,
                active_issuers,
                credentials,
            },
            _credential_writer: devices,
        })
    }

    pub(in crate::node) fn publish_policy(
        &self,
        controller: Option<Issuer>,
        prepared: Option<PreparedPolicy<'_>>,
    ) -> PolicyPublication {
        let mut state = self.write();
        let controller_changed = state.controller != controller;
        state.controller = controller;
        let Some(mut prepared) = prepared else {
            return PolicyPublication {
                changed: controller_changed,
                _retired: None,
            };
        };
        std::mem::swap(&mut state.policy, &mut prepared.next.policy);
        std::mem::swap(&mut state.authorities, &mut prepared.next.authorities);
        std::mem::swap(&mut state.active_issuers, &mut prepared.next.active_issuers);
        std::mem::swap(&mut state.credentials, &mut prepared.next.credentials);
        drop(state);

        PolicyPublication {
            changed: true,
            _retired: Some(prepared.next),
        }
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
            let controller = self.read().controller;
            self.publish_policy(controller, Some(prepared));
            self.notify_policy_changed();
        }
        Ok(())
    }
}
