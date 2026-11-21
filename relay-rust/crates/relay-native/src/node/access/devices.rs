#[cfg(test)]
#[path = "devices_tests.rs"]
mod tests;

use super::Service;
use std::{
    collections::BTreeMap,
    ops::Bound,
    sync::{Arc, Mutex, MutexGuard},
};
use windshare_relay_access::{
    AccountId, Credential, CredentialId, CredentialState, Digest, Issuer, trust::Generation,
};

#[derive(Clone, Copy, PartialEq, Eq)]
pub struct BoundAccount {
    pub issuer: Issuer,
    pub account: AccountId,
}

pub struct DeviceCredential {
    pub key: Credential,
    pub account: Option<BoundAccount>,
}

#[derive(Default)]
pub struct DeviceKeys(pub Vec<DeviceCredential>);

pub(super) struct CredentialEntry {
    pub value: Credential,
    // Both indexes observe registrations made while a replacement index is built.
    last_use: Arc<Mutex<Option<u64>>>,
}

impl CredentialEntry {
    pub(super) fn record_use(&self, now: u64) {
        let mut last_use = self.last_use.lock().expect("credential use");
        *last_use = (*last_use).max(Some(now));
    }

    fn snapshot(&self) -> Credential {
        let last_use = *self.last_use.lock().expect("credential use");
        let mut credential = self.value.clone();
        credential.last_used_at = credential.last_used_at.max(last_use);
        credential
    }
}

#[derive(Default)]
pub(super) struct CredentialIndex {
    entries: Vec<CredentialEntry>,
    by_id: BTreeMap<CredentialId, usize>,
    by_digest: BTreeMap<[u8; 32], usize>,
    accounts: BTreeMap<CredentialId, BoundAccount>,
    active_entries: Vec<usize>,
    revoked_entries: BTreeMap<(u64, CredentialId), usize>,
}

impl CredentialIndex {
    pub(super) fn build(configured: &[Credential], devices: &DeviceKeys, previous: &Self) -> Self {
        let mut index = Self::default();
        for credential in configured {
            index.insert(credential.clone(), previous);
        }
        for device in &devices.0 {
            let key = &device.key;
            if let Some(account) = device.account {
                index.accounts.insert(key.id, account);
            }
            let by_id = index.by_id.get(&key.id).copied();
            let by_digest = index.by_digest.get(&key.digest.0).copied();
            // Either the configured identifier or the key material can survive a reload.
            if matches!(key.state, CredentialState::Revoked { .. }) {
                for entry in by_id.into_iter().chain(by_digest) {
                    index.entries[entry].value.state = key.state;
                }
            }
            if by_id.is_none() && by_digest.is_none() {
                index.insert(key.clone(), previous);
            }
        }
        for (entry, credential) in index.entries.iter().enumerate() {
            let key = &credential.value;
            match key.state {
                CredentialState::Active => index.active_entries.push(entry),
                CredentialState::Revoked { at } => {
                    index.revoked_entries.insert((at, key.id), entry);
                }
            }
        }
        index
    }

    fn insert(&mut self, mut credential: Credential, previous: &Self) {
        let index = self.entries.len();
        self.by_id.entry(credential.id).or_insert(index);
        self.by_digest.entry(credential.digest.0).or_insert(index);
        let last_use = match previous.by_id(credential.id) {
            Some(previous) => {
                credential.last_used_at = credential.last_used_at.max(previous.value.last_used_at);
                Arc::clone(&previous.last_use)
            }
            None => Arc::new(Mutex::new(None)),
        };
        self.entries.push(CredentialEntry {
            value: credential,
            last_use,
        });
    }

    fn by_id(&self, id: CredentialId) -> Option<&CredentialEntry> {
        self.by_id.get(&id).map(|&index| &self.entries[index])
    }

    pub(super) fn active(&self, id: CredentialId) -> bool {
        self.by_id(id)
            .is_some_and(|entry| entry.value.state == CredentialState::Active)
    }

    pub(super) fn account(&self, id: CredentialId) -> Option<BoundAccount> {
        self.accounts.get(&id).copied()
    }

    pub(super) fn get(&self, digest: Digest) -> Option<&CredentialEntry> {
        self.by_digest
            .get(&digest.0)
            .map(|&index| &self.entries[index])
    }

    fn active_inventory(&self) -> impl Iterator<Item = &CredentialEntry> {
        self.active_entries
            .iter()
            .map(|&entry| &self.entries[entry])
    }

    fn revoked_before(
        &self,
        after: Option<(u64, CredentialId)>,
    ) -> impl Iterator<Item = &CredentialEntry> {
        let before = after.map_or(Bound::Unbounded, Bound::Excluded);
        self.revoked_entries
            .range((Bound::Unbounded, before))
            .rev()
            .map(|(_, &entry)| &self.entries[entry])
    }
}

impl Service {
    pub(crate) fn observe_device_login(&self, issuer: Issuer, supported: bool) {
        let mut state = self.write();
        if state.controller == Some(issuer) {
            state.login_issuer = supported.then_some(issuer);
        }
    }

    pub(crate) fn login_generation(&self, issuer: Issuer, now: u64) -> Option<Generation> {
        let state = self.read();
        if state.login_issuer != Some(issuer)
            || state.controller != Some(issuer)
            || !state.active_issuers.contains(&issuer)
            || !state
                .authorities
                .policies
                .get(&issuer)
                .is_some_and(|policy| policy.enabled)
        {
            return None;
        }
        state
            .authorities
            .trusted
            .generation(issuer)
            .filter(|generation| generation.usable(now))
    }

    pub(crate) fn restore_devices(&self, restored: DeviceKeys) {
        let mut devices = self.devices.lock().expect("credential writer");
        *devices = restored;
        self.publish_credentials(devices);
    }

    pub(crate) fn apply_device(&self, device: DeviceCredential) {
        let id = device.key.id;
        let mut devices = self.devices.lock().expect("credential writer");
        // Preserve the same order as the durable inventory when a key is overwritten.
        match devices.0.iter_mut().find(|entry| entry.key.id == id) {
            Some(entry) => *entry = device,
            None => devices.0.push(device),
        }
        self.publish_credentials(devices);
    }

    fn publish_credentials(&self, devices: MutexGuard<'_, DeviceKeys>) {
        let (policy, previous) = self.credential_snapshot();
        let next = Arc::new(CredentialIndex::build(
            &policy.credentials,
            &devices,
            &previous,
        ));
        self.write().credentials = next;
        drop(devices);
        self.changes.send_modify(|version| *version += 1);
    }

    pub(crate) fn credential_by_id(&self, id: CredentialId) -> Option<Credential> {
        self.credential_index()
            .by_id(id)
            .map(CredentialEntry::snapshot)
    }

    pub(crate) fn has_credential(
        &self,
        id: CredentialId,
        matches_state: impl Fn(CredentialState) -> bool,
    ) -> bool {
        self.credential_index()
            .entries
            .iter()
            .any(|entry| entry.value.id == id && matches_state(entry.value.state))
    }

    pub(crate) fn active_credentials(&self) -> Vec<Credential> {
        self.credential_index()
            .active_inventory()
            .map(CredentialEntry::snapshot)
            .collect()
    }

    pub(crate) fn revoked_credentials(
        &self,
        after: Option<(u64, CredentialId)>,
        limit: usize,
    ) -> Vec<Credential> {
        self.credential_index()
            .revoked_before(after)
            .take(limit)
            .map(CredentialEntry::snapshot)
            .collect()
    }

    fn credential_index(&self) -> Arc<CredentialIndex> {
        Arc::clone(&self.read().credentials)
    }

    pub(super) fn credential_snapshot(&self) -> (Arc<super::Policy>, Arc<CredentialIndex>) {
        let state = self.read();
        (Arc::clone(&state.policy), Arc::clone(&state.credentials))
    }
}
