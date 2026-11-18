use super::{Service, State};
use std::collections::BTreeMap;
use windshare_relay_access::{
    AccountId, Credential, CredentialId, CredentialState, Issuer, trust::Generation,
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
            .clone()
    }
}

impl State {
    pub(super) fn rebuild_credentials(&mut self) {
        let previous = std::mem::take(&mut self.credentials);
        self.credentials.clone_from(&self.policy.credentials);
        for device in &self.devices.keys {
            // Either the configured identifier or the key material can survive a reload.
            if matches!(device.state, CredentialState::Revoked { .. }) {
                for c in &mut self.credentials {
                    if c.id == device.id || c.digest == device.digest {
                        c.state = device.state;
                    }
                }
            }
            if !self
                .credentials
                .iter()
                .any(|c| c.id == device.id || c.digest == device.digest)
            {
                self.credentials.push(device.clone());
            }
        }
        for c in &mut self.credentials {
            if let Some(prior) = previous.iter().find(|p| p.id == c.id) {
                c.last_used_at = c.last_used_at.max(prior.last_used_at);
            }
        }
    }
}
