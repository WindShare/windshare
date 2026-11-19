//! Local admission and entitlement decisions; authority I/O never enters a share actor.
mod cache;
mod devices;
mod policy;
pub(crate) use cache::Cache;
pub(crate) use devices::{BoundAccount, DeviceKeys};
mod residency;
#[cfg(test)]
#[path = "access_tests.rs"]
mod tests;
pub use policy::{Policy, TrustedIssuer};
pub(crate) use residency::Residency;

use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
};
use tokio::sync::watch;
use windshare_relay_access::{
    AccountId, Attribution, CredentialState, Digest, Issuer, Principal,
    entitlement::{self, Allowance, Class, Entitlement},
    restrictions::Restrictions,
    ticket::Ticket,
    trust::TrustStore,
};
use windshare_relay_core::{access::RegistrationDecision, identifiers::ShareId};

#[derive(Clone)]
pub(crate) struct Service {
    state: Arc<Mutex<State>>,
    changes: watch::Sender<u64>,
}

struct State {
    policy: Policy,
    node_policies: BTreeMap<Issuer, windshare_relay_access::usage::NodePolicy>,
    controller: Option<Issuer>,
    login_issuer: Option<Issuer>,
    trusted: TrustStore,
    active_issuers: std::collections::BTreeSet<Issuer>,
    devices: DeviceKeys,
    credentials: devices::CredentialIndex,
    owners: Vec<[u8; 16]>,
    entitlements: BTreeMap<(Issuer, AccountId), Entitlement>,
    restrictions: BTreeMap<Issuer, Restrictions>,
    residents: BTreeMap<ShareId, (Attribution, usize)>,
}

impl Default for Service {
    fn default() -> Self {
        Self {
            state: Arc::new(Mutex::new(State {
                policy: Policy::default(),
                node_policies: BTreeMap::new(),
                controller: None,
                login_issuer: None,
                trusted: TrustStore::default(),
                active_issuers: std::collections::BTreeSet::new(),
                devices: DeviceKeys::default(),
                credentials: devices::CredentialIndex::default(),
                owners: Vec::new(),
                entitlements: BTreeMap::new(),
                restrictions: BTreeMap::new(),
                residents: BTreeMap::new(),
            })),
            changes: watch::channel(0).0,
        }
    }
}

impl Service {
    pub fn set_controller(&self, issuer: Option<Issuer>) {
        self.state.lock().expect("node access state").controller = issuer;
    }

    pub fn public_enabled(&self) -> bool {
        let state = self.state.lock().expect("node access state");
        state
            .controller
            .filter(|issuer| state.active_issuers.contains(issuer))
            .and_then(|issuer| state.node_policies.get(&issuer))
            .is_none_or(|policy| policy.enabled)
    }

    pub fn anonymous(&self) -> bool {
        self.state.lock().expect("node access state").anonymous()
    }

    pub fn priority(
        &self,
        share: ShareId,
        bearer: Option<&str>,
        now: u64,
    ) -> crate::connection::AdmissionPriority {
        let state = self.state.lock().expect("node access state");
        let resident_owner = state.residents.get(&share).is_some_and(|(attribution, _)| {
            state.owner(*attribution) && !state.denied(share, *attribution)
        });
        let resident_authenticated = state.residents.get(&share).is_some_and(|(attribution, _)| {
            !matches!(attribution, Attribution::Anonymous) && !state.denied(share, *attribution)
        });
        let token_owner = bearer.is_some_and(|token| {
            let digest = Digest::of(token.as_bytes());
            state.credentials.get(digest).is_some_and(|credential| {
                credential.state == CredentialState::Active
                    && !state.devices.accounts.contains_key(&credential.id)
                    && state.owners.contains(&credential.principal)
            })
        });
        drop(state);
        if resident_owner || token_owner {
            crate::connection::AdmissionPriority::Owner
        } else if resident_authenticated
            || bearer.is_some()
                && matches!(
                    self.authorize(share, bearer, now),
                    RegistrationDecision::Granted(_)
                )
        {
            crate::connection::AdmissionPriority::Authenticated
        } else {
            crate::connection::AdmissionPriority::Anonymous
        }
    }

    pub fn share_limit(&self, priority: crate::connection::AdmissionPriority) -> usize {
        self.state
            .lock()
            .expect("node access state")
            .share_limit(priority)
    }

    pub fn changes(&self) -> watch::Receiver<u64> {
        self.changes.subscribe()
    }

    pub fn authorize(
        &self,
        share: ShareId,
        bearer: Option<&str>,
        now: u64,
    ) -> RegistrationDecision {
        let mut state = self.state.lock().expect("node access state");
        let attribution = match bearer {
            None if state.anonymous() => Attribution::Anonymous,
            None => return RegistrationDecision::Denied,
            Some(token) => {
                let digest = Digest::of(token.as_bytes());
                if let Some(credential) = state.credentials.get_mut(digest) {
                    let Ok(attribution) = credential.authorize(digest, now.saturating_mul(1000))
                    else {
                        return RegistrationDecision::Denied;
                    };
                    let id = credential.id;
                    state
                        .devices
                        .accounts
                        .get(&id)
                        .map_or(attribution, |binding| Attribution::Account {
                            issuer: binding.issuer,
                            account: binding.account,
                            credential: id,
                        })
                } else {
                    let Some(ticket) = URL_SAFE_NO_PAD
                        .decode(token)
                        .ok()
                        .and_then(|bytes| Ticket::verify(&bytes, &state.trusted, now).ok())
                    else {
                        return RegistrationDecision::Denied;
                    };
                    if !state.active_issuers.contains(&ticket.issuer) {
                        return RegistrationDecision::Denied;
                    }
                    let cached = state
                        .entitlements
                        .get(&(ticket.issuer, ticket.account))
                        .copied();
                    let mut next = cached;
                    if entitlement::advance(&mut next, ticket.entitlement) {
                        state
                            .entitlements
                            .insert((ticket.issuer, ticket.account), ticket.entitlement);
                    }
                    ticket.attribution()
                }
            }
        };
        if state.denied(share, attribution) {
            return RegistrationDecision::Denied;
        }
        drop(state);
        RegistrationDecision::Granted(attribution)
    }

    pub fn with_forwarding<T>(
        &self,
        share: ShareId,
        attribution: Attribution,
        forward: impl FnOnce() -> T,
    ) -> Option<T> {
        let state = self.state.lock().expect("node access state");
        if state.denied(share, attribution) {
            return None;
        }
        let result = forward();
        drop(state);
        Some(result)
    }

    pub fn allowed(&self, share: ShareId, attribution: Attribution) -> bool {
        !self
            .state
            .lock()
            .expect("node access state")
            .denied(share, attribution)
    }

    pub fn class(&self, attribution: Attribution, now: u64) -> (Class, Option<Allowance>) {
        let state = self.state.lock().expect("node access state");
        let result = match attribution {
            Attribution::Credential { principal, .. } if state.owners.contains(&principal) => {
                (Class::Owner, None)
            }
            Attribution::Credential { .. } => (Class::Standard, None),
            Attribution::Anonymous => (Class::Lowest, None),
            Attribution::Account {
                issuer, account, ..
            } => {
                let allowance = state
                    .entitlements
                    .get(&(issuer, account))
                    .map_or(Allowance::Stale, |entitlement| entitlement.allowance(now));
                (Entitlement::class(allowance), Some(allowance))
            }
        };
        drop(state);
        result
    }

    pub fn refresh(&self, issuer: Issuer, account: AccountId, next: Entitlement) -> bool {
        let mut state = self.state.lock().expect("node access state");
        let mut previous = state.entitlements.get(&(issuer, account)).copied();
        let changed = entitlement::advance(&mut previous, next);
        if changed {
            state.entitlements.insert((issuer, account), next);
        }
        drop(state);
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

    pub fn restriction_version(&self, issuer: Issuer) -> u64 {
        self.state
            .lock()
            .expect("node access state")
            .restrictions
            .get(&issuer)
            .map_or(0, |value| value.version)
    }

    pub fn restrict(&self, issuer: Issuer, next: Restrictions) -> bool {
        let mut state = self.state.lock().expect("node access state");
        let version = next.version;
        let changed = state.restrictions.entry(issuer).or_default().advance(next);
        drop(state);
        if changed {
            self.changes.send_modify(|value| *value += 1);
            tracing::warn!(event = "access_restrictions_updated", ?issuer, version);
        }
        changed
    }

    pub fn resident_shares(
        &self,
        authority: Issuer,
    ) -> Vec<windshare_relay_access::usage::ShareAttribution> {
        self.state
            .lock()
            .expect("node access state")
            .residents
            .iter()
            .filter_map(|(share, (attribution, _))| match attribution {
                Attribution::Account {
                    issuer, account, ..
                } if *issuer == authority => {
                    Some(windshare_relay_access::usage::ShareAttribution {
                        share: *share.as_bytes(),
                        account: *account,
                    })
                }
                _ => None,
            })
            .collect()
    }

    pub fn resident_accounts(&self) -> Vec<(Issuer, AccountId)> {
        self.state
            .lock()
            .expect("node access state")
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
            .collect()
    }
}

impl State {
    const fn share_limit(&self, priority: crate::connection::AdmissionPriority) -> usize {
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

    fn anonymous(&self) -> bool {
        self.controller
            .filter(|issuer| self.active_issuers.contains(issuer))
            .and_then(|issuer| self.node_policies.get(&issuer))
            .map_or(self.policy.anonymous, |policy| {
                policy.enabled && policy.anonymous
            })
    }

    fn denied(&self, share: ShareId, attribution: Attribution) -> bool {
        let disabled = self
            .controller
            .filter(|issuer| self.active_issuers.contains(issuer))
            .and_then(|issuer| {
                self.node_policies
                    .get(&issuer)
                    .map(|policy| (issuer, policy))
            })
            .is_some_and(|(controller, policy)| {
                !policy.enabled
                    && match attribution {
                        Attribution::Anonymous => true,
                        Attribution::Account { issuer, .. } => issuer == controller,
                        Attribution::Credential { .. } => false,
                    }
            });
        let local_key = match attribution {
            Attribution::Credential { credential, .. } => Some(credential),
            Attribution::Account {
                issuer,
                account,
                credential,
            } if self.devices.accounts.get(&credential)
                == Some(&BoundAccount { issuer, account }) =>
            {
                Some(credential)
            }
            _ => None,
        };
        disabled
            || local_key.is_some_and(|id| !self.credentials.active(id))
            || matches!(attribution, Attribution::Account { issuer, .. } if !self.active_issuers.contains(&issuer))
            || self.restrictions.iter().any(|(issuer, restrictions)| {
                restrictions.denied(*issuer, *share.as_bytes(), attribution)
            })
    }

    fn owner(&self, attribution: Attribution) -> bool {
        matches!(attribution, Attribution::Credential { principal, .. } if self.owners.contains(&principal))
    }
}
