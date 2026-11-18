use super::{Service, State};
use windshare_relay_access::{Credential, CredentialState};

#[derive(Default)]
pub struct DeviceKeys(pub Vec<Credential>);

impl Service {
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
        for device in &self.devices.0 {
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
