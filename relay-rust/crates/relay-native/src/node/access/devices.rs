use super::{Service, State};
use std::collections::BTreeMap;
use windshare_relay_access::{
    AccountId, Credential, CredentialId, CredentialState, Digest, Issuer, trust::Generation,
};

#[derive(Clone, Copy, PartialEq, Eq)]
pub struct BoundAccount {
    pub issuer: Issuer,
    pub account: AccountId,
}

#[derive(Default)]
pub struct DeviceKeys {
    pub keys: Vec<Credential>,
    pub accounts: BTreeMap<CredentialId, BoundAccount>,
}

#[derive(Default)]
pub(super) struct CredentialIndex {
    entries: Vec<Credential>,
    by_id: BTreeMap<CredentialId, usize>,
    by_digest: BTreeMap<[u8; 32], usize>,
}

impl CredentialIndex {
    fn insert(&mut self, credential: Credential) {
        let index = self.entries.len();
        self.by_id.entry(credential.id).or_insert(index);
        self.by_digest.entry(credential.digest.0).or_insert(index);
        self.entries.push(credential);
    }

    pub(super) fn active(&self, id: CredentialId) -> bool {
        self.by_id
            .get(&id)
            .is_some_and(|&index| self.entries[index].state == CredentialState::Active)
    }

    pub(super) fn get(&self, digest: Digest) -> Option<&Credential> {
        self.by_digest
            .get(&digest.0)
            .map(|&index| &self.entries[index])
    }

    pub(super) fn get_mut(&mut self, digest: Digest) -> Option<&mut Credential> {
        self.by_digest
            .get(&digest.0)
            .map(|&index| &mut self.entries[index])
    }
}

impl Service {
    pub(crate) fn observe_device_login(&self, issuer: Issuer, supported: bool) {
        let mut state = self.state.lock().expect("node access state");
        if state.controller == Some(issuer) {
            state.login_issuer = supported.then_some(issuer);
        }
    }
    pub(crate) fn login_generation(&self, issuer: Issuer, now: u64) -> Option<Generation> {
        let state = self.state.lock().expect("node access state");
        if state.login_issuer != Some(issuer)
            || state.controller != Some(issuer)
            || !state.active_issuers.contains(&issuer)
            || !state
                .node_policies
                .get(&issuer)
                .is_some_and(|policy| policy.enabled)
        {
            return None;
        }
        state
            .trusted
            .generations()
            .into_iter()
            .find(|g| g.issuer == issuer && g.usable(now))
    }
    pub(crate) fn apply_devices(&self, devices: DeviceKeys) {
        let mut state = self.state.lock().expect("node access state");
        state.devices = devices;
        state.rebuild_credentials();
        drop(state);
        self.changes.send_modify(|version| *version += 1);
    }

    pub(crate) fn credential_inventory(&self) -> Vec<Credential> {
        self.state
            .lock()
            .expect("node access state")
            .credentials
            .entries
            .clone()
    }
}

impl State {
    pub(super) fn rebuild_credentials(&mut self) {
        let previous = std::mem::take(&mut self.credentials);
        for credential in &self.policy.credentials {
            self.credentials.insert(credential.clone());
        }
        for device in &self.devices.keys {
            let by_id = self.credentials.by_id.get(&device.id).copied();
            let by_digest = self.credentials.by_digest.get(&device.digest.0).copied();
            // Either the configured identifier or the key material can survive a reload.
            if matches!(device.state, CredentialState::Revoked { .. }) {
                for index in by_id.into_iter().chain(by_digest) {
                    self.credentials.entries[index].state = device.state;
                }
            }
            if by_id.is_none() && by_digest.is_none() {
                self.credentials.insert(device.clone());
            }
        }
        for c in &mut self.credentials.entries {
            if let Some(&index) = previous.by_id.get(&c.id) {
                c.last_used_at = c.last_used_at.max(previous.entries[index].last_used_at);
            }
        }
    }
}
