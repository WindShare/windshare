use super::{Service, TrustStore};
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Serialize};
use windshare_relay_access::{Credential, trust::Generation};

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

impl Service {
    pub fn policy_matches(&self, policy: &Policy) -> bool {
        self.state.lock().expect("node access state").policy == *policy
    }
    /// # Errors
    /// Rejects invalid admission limits, issuer certificates and signing-key rollback.
    pub fn configure(&self, policy: Policy) -> Result<()> {
        ensure!(
            policy.shares_per_principal > 0
                && policy.owner_reserved_shares < crate::config::MAX_RESIDENT_SHARES,
            "invalid resident admission limits"
        );
        let mut state = self
            .state
            .lock()
            .map_err(|_| anyhow!("node access state poisoned"))?;
        if state.policy == policy {
            return Ok(());
        }
        let mut trusted: TrustStore = state.trusted.clone();
        let mut active_issuers = std::collections::BTreeSet::new();
        for issuer in &policy.trusted_issuers {
            let root = URL_SAFE_NO_PAD
                .decode(&issuer.root)?
                .try_into()
                .map_err(|_| anyhow!("issuer root must be an Ed25519 public key"))?;
            ensure!(active_issuers.insert(root), "duplicate trusted issuer");
            let certificate = URL_SAFE_NO_PAD.decode(&issuer.certificate)?;
            let generation = Generation::verify(&certificate, root)
                .map_err(|error| anyhow!("issuer certificate: {error:?}"))?;
            if trusted
                .generations()
                .iter()
                .any(|current| current.issuer == root && current.number > generation.number)
            {
                continue;
            }
            trusted
                .remember(generation)
                .map_err(|error| anyhow!("issuer generation: {error:?}"))?;
        }
        state.trusted = trusted;
        state.active_issuers = active_issuers;
        state.owners.clone_from(&policy.owner_principals);
        state.policy = policy;
        state.rebuild_credentials();
        drop(state);
        self.changes.send_modify(|version| *version += 1);
        Ok(())
    }
}
