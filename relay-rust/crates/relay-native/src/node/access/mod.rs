//! Local admission and entitlement decisions; authority I/O never enters a share actor.
mod admission;
mod control;
mod devices;
mod policy;
mod quota;
pub(crate) use admission::{Authorization, Bearer, Refusal, RefusalReason};
pub(crate) use control::{AccessSnapshot, AccessUpdate, AuthorityState};
use devices::CredentialIndex;
pub(crate) use devices::{BoundAccount, DeviceCredential, DeviceKeys};
pub(super) use policy::PreparedPolicy;
mod residency;
#[cfg(test)]
#[path = "access_tests.rs"]
mod tests;
pub use policy::{Policy, TrustedIssuer};
use quota::Quotas;
pub(crate) use quota::{AccountKey, Quota};
pub(crate) use residency::Residency;
use windshare_relay_access::usage::{Admission, NodePolicy};

use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex, RwLock, RwLockReadGuard, RwLockWriteGuard},
};
use tokio::sync::watch;
use windshare_relay_access::{
    AccountId, Attribution, Issuer,
    entitlement::{Allowance, Class, DAY_SECONDS, Entitlement},
    restrictions::Restrictions,
};
use windshare_relay_core::identifiers::ShareId;

#[derive(Clone)]
pub(crate) struct Service {
    // Read-mostly access checks (including concurrent frame submission) share
    // this lock while policy, restriction, quota, and residency mutations fence them.
    state: Arc<RwLock<State>>,
    // Serializes the writers of the effective credential index, including policy reloads.
    devices: Arc<Mutex<DeviceKeys>>,
    changes: watch::Sender<u64>,
}

struct State {
    policy: Arc<Policy>,
    authorities: control::Authorities,
    controller: Option<Issuer>,
    login_issuer: Option<Issuer>,
    active_issuers: std::collections::BTreeSet<Issuer>,
    credentials: Arc<CredentialIndex>,
    quotas: Quotas,
    restrictions: BTreeMap<Issuer, Restrictions>,
    residents: BTreeMap<ShareId, (Attribution, usize)>,
}

impl Default for Service {
    fn default() -> Self {
        Self {
            state: Arc::new(RwLock::new(State {
                policy: Arc::default(),
                authorities: control::Authorities::default(),
                controller: None,
                login_issuer: None,
                active_issuers: std::collections::BTreeSet::new(),
                credentials: Arc::default(),
                quotas: Quotas::default(),
                restrictions: BTreeMap::new(),
                residents: BTreeMap::new(),
            })),
            devices: Arc::default(),
            changes: watch::channel(0).0,
        }
    }
}

impl Service {
    fn read(&self) -> RwLockReadGuard<'_, State> {
        self.state.read().expect("node access state")
    }

    fn write(&self) -> RwLockWriteGuard<'_, State> {
        self.state.write().expect("node access state")
    }

    #[cfg(test)]
    pub fn set_controller(&self, issuer: Option<Issuer>) {
        self.write().controller = issuer;
    }

    pub fn public_enabled(&self) -> bool {
        self.read().admission() == Admission::Accepting
    }

    pub fn reported_policy(
        &self,
        issuer: Issuer,
        mut durable: BTreeMap<ShareId, Attribution>,
    ) -> (u64, u64) {
        let state = self.read();
        for (&share, &(attribution, _)) in &state.residents {
            durable.entry(share).or_insert(attribution);
        }
        let resident_shares = durable.len() as u64;
        let version = state
            .authorities
            .policies
            .get(&issuer)
            .map_or(0, |p| p.version);
        drop(state);
        (version, resident_shares)
    }

    pub fn anonymous(&self) -> bool {
        self.read().anonymous()
    }

    pub fn share_limit(&self, priority: crate::connection::AdmissionPriority) -> usize {
        self.read().share_limit(priority)
    }

    pub fn changes(&self) -> watch::Receiver<u64> {
        self.changes.subscribe()
    }

    pub fn with_forwarding<T>(
        &self,
        share: ShareId,
        attribution: Attribution,
        forward: impl FnOnce() -> T,
    ) -> Option<T> {
        let state = self.read();
        if state.denied(share, attribution) {
            return None;
        }
        let result = forward();
        drop(state);
        Some(result)
    }

    pub fn allowed(&self, share: ShareId, attribution: Attribution) -> bool {
        !self.read().denied(share, attribution)
    }

    pub(crate) fn is_owner(&self, attribution: Attribution) -> bool {
        self.read().owner(attribution)
    }

    pub fn class(&self, attribution: Attribution, now: u64) -> (Class, Option<Allowance>) {
        self.read().class(attribution, now)
    }

    pub fn refresh(&self, issuer: Issuer, account: AccountId, next: Entitlement, now: u64) -> bool {
        let changed =
            self.write()
                .quotas
                .advance((issuer, account), Quota::from_snapshot(next, now), now);
        if changed {
            self.changes.send_modify(|version| *version += 1);
            tracing::info!(
                event = "entitlement_refreshed",
                ?issuer,
                ?account,
                version = next.version,
                day = next.day,
                valid_until = next.valid_until,
                remaining_bytes = next.remaining_bytes
            );
        }
        changed
    }

    pub(super) fn expire_quotas(&self, now: u64) {
        let day = now / DAY_SECONDS;
        // Skip the exclusive lock on the 1s scheduler tick when no quota deadline is due.
        if !self.read().quotas.needs_retire(day, now) {
            return;
        }
        let pruned = self.write().quotas.retire_before(day, now);
        if pruned != 0 {
            tracing::debug!(event = "runtime_quotas_pruned", day, pruned);
        }
    }

    pub fn restriction_version(&self, issuer: Issuer) -> u64 {
        self.read()
            .restrictions
            .get(&issuer)
            .map_or(0, |value| value.version)
    }

    pub fn restrict(&self, issuer: Issuer, next: Restrictions) -> bool {
        let mut state = self.write();
        let version = next.version;
        let changed = state.restrictions.entry(issuer).or_default().advance(next);
        drop(state);
        if changed {
            self.changes.send_modify(|value| *value += 1);
            tracing::warn!(event = "access_restrictions_updated", ?issuer, version);
        }
        changed
    }

    pub fn share_restricted(&self, share: ShareId) -> bool {
        self.read().share_restricted(share)
    }

    pub fn resident_accounts(&self) -> Vec<(Issuer, AccountId)> {
        let state = self.read();
        let accounts = state
            .residents
            .values()
            .filter_map(|(attribution, _)| match attribution {
                Attribution::Account {
                    issuer, account, ..
                } => Some((*issuer, *account)),
                _ => None,
            })
            .collect::<std::collections::BTreeSet<_>>()
            .into_iter()
            .collect();
        drop(state);
        accounts
    }
}

impl State {
    fn share_restricted(&self, share: ShareId) -> bool {
        self.restrictions
            .values()
            .any(|restrictions| restrictions.shares.contains(share.as_bytes()))
    }

    fn share_limit(&self, priority: crate::connection::AdmissionPriority) -> usize {
        use crate::connection::AdmissionPriority;
        let public = crate::config::MAX_RESIDENT_SHARES - self.policy.owner_reserved_shares;
        match priority {
            AdmissionPriority::Owner => crate::config::MAX_RESIDENT_SHARES,
            AdmissionPriority::Authenticated => public,
            AdmissionPriority::Anonymous => {
                public - public / policy::AUTHENTICATED_CAPACITY_DIVISOR
            }
        }
    }

    fn controller_policy(&self) -> Option<&NodePolicy> {
        self.controller
            .filter(|i| self.active_issuers.contains(i))
            .and_then(|i| self.authorities.policies.get(&i))
    }

    fn admission(&self) -> Admission {
        match (self.controller, self.controller_policy()) {
            (_, Some(policy)) => policy.admission,
            // An official install needs its first signed approval before admitting users.
            (Some(_), None) => Admission::Draining,
            (None, None) => Admission::Accepting,
        }
    }

    fn anonymous(&self) -> bool {
        self.admission() == Admission::Accepting
            && self
                .controller_policy()
                .map_or(self.policy.anonymous, |p| p.anonymous)
    }

    pub(super) fn class(&self, attribution: Attribution, now: u64) -> (Class, Option<Allowance>) {
        match attribution {
            Attribution::Credential { .. } if self.owner(attribution) => (Class::Owner, None),
            Attribution::Credential { .. } => (Class::Standard, None),
            Attribution::Anonymous => (Class::Lowest, None),
            Attribution::Account {
                issuer, account, ..
            } => {
                let allowance = self.quotas.allowance((issuer, account), now);
                (Entitlement::class(allowance), Some(allowance))
            }
        }
    }

    pub(super) fn denial_reason(
        &self,
        share: ShareId,
        attribution: Attribution,
    ) -> Option<RefusalReason> {
        if self.admission() == Admission::Disabled {
            return Some(RefusalReason::NodePublicDisabled);
        }
        let local_key = match attribution {
            Attribution::Credential { credential, .. } => Some(credential),
            Attribution::Account {
                issuer,
                account,
                credential,
            } if self.credentials.account(credential) == Some(BoundAccount { issuer, account }) => {
                Some(credential)
            }
            _ => None,
        };
        if local_key.is_some_and(|id| !self.credentials.active(id)) {
            return Some(RefusalReason::CredentialInactive);
        }
        if matches!(attribution, Attribution::Account { issuer, .. } if !self.active_issuers.contains(&issuer))
        {
            return Some(RefusalReason::IssuerInactive);
        }
        for (issuer, restrictions) in &self.restrictions {
            if restrictions.shares.contains(share.as_bytes()) {
                return Some(RefusalReason::ShareRestricted);
            }
            if let Attribution::Account {
                issuer: actual,
                account,
                credential,
            } = attribution
                && actual == *issuer
            {
                if restrictions.accounts.contains(&account) {
                    return Some(RefusalReason::AccountRestricted);
                }
                if restrictions.credentials.contains(&credential) {
                    return Some(RefusalReason::CredentialRestricted);
                }
            }
        }
        None
    }

    fn denied(&self, share: ShareId, attribution: Attribution) -> bool {
        self.denial_reason(share, attribution).is_some()
    }

    fn owner(&self, attribution: Attribution) -> bool {
        matches!(attribution, Attribution::Credential { principal, .. } if self.policy.owner_principals.contains(&principal))
    }

    fn priority_for(&self, attribution: Attribution) -> crate::connection::AdmissionPriority {
        use crate::connection::AdmissionPriority;
        if self.owner(attribution) {
            AdmissionPriority::Owner
        } else if matches!(attribution, Attribution::Anonymous) {
            AdmissionPriority::Anonymous
        } else {
            AdmissionPriority::Authenticated
        }
    }
}
