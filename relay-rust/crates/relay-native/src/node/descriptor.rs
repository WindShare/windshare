use super::{Identity, Runtime, Services, usage::Reporter};
use anyhow::{Result, anyhow};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use ed25519_dalek::SigningKey;
use windshare_relay_access::{Credential, trust};

const DESCRIPTOR_DOMAIN: &[u8] = b"windshare/node-descriptor/v2\0";
const DESCRIPTOR_SECONDS: u64 = 300;

/// Immutable node identity installed once at startup with pre-derived key material
/// so discovery and status checks never re-expand the seed or lock `Runtime::state`.
pub(super) struct InstalledIdentity {
    raw: Identity,
    signing_key: SigningKey,
    id: String,
    key_b64: String,
}

impl InstalledIdentity {
    fn new(raw: Identity) -> Self {
        let signing_key = raw.key();
        let verifying_key = signing_key.verifying_key().to_bytes();
        let id = super::identity::node_id(&verifying_key);
        let key_b64 = URL_SAFE_NO_PAD.encode(verifying_key);
        Self {
            raw,
            signing_key,
            id,
            key_b64,
        }
    }

    pub(super) const fn raw(&self) -> &Identity {
        &self.raw
    }

    pub(super) const fn signing_key(&self) -> &SigningKey {
        &self.signing_key
    }

    pub(super) fn id(&self) -> &str {
        &self.id
    }

    pub(super) fn key_b64(&self) -> &str {
        &self.key_b64
    }

    pub(super) fn owner(&self) -> Credential {
        self.raw.owner()
    }
}

struct DescriptorSnapshot {
    services: Services,
    stun_urls: Vec<String>,
    zone: String,
    region: String,
    capacity_bytes_per_second: Option<u64>,
    authority: Option<super::usage::Authority>,
    relay_ready: bool,
}

impl Runtime {
    /// # Errors
    /// The identity must be installed once before ingress starts.
    pub fn identify(&self, identity: Identity) -> Result<()> {
        self.identity
            .set(InstalledIdentity::new(identity))
            .map_err(|_| anyhow!("node identity already installed"))
    }

    pub(crate) fn open_console(
        &self,
        endpoint: &crate::Endpoint,
        store: &crate::Database,
    ) -> Result<()> {
        let Some(identity) = self.identity.get() else {
            return Ok(());
        };
        let authority = self
            .state
            .lock()
            .map_err(|_| anyhow!("node state"))?
            .settings
            .authority
            .clone();
        if let Some(console) = crate::console::Console::open(
            endpoint,
            store,
            self.access.clone(),
            identity.raw(),
            authority,
        )? {
            self.console
                .set(console)
                .map_err(|_| anyhow!("console already initialized"))?;
        }
        Ok(())
    }

    pub(crate) fn reporter(&self) -> Result<Option<Reporter>> {
        let writer = self.lock_control()?;
        let state = self.state.lock().map_err(|_| anyhow!("node state"))?;
        self.reporting_authority
            .get_or_init(|| state.settings.authority.clone());
        let Some(authority) = state.settings.authority.clone() else {
            return Ok(None);
        };
        let store = state.control.clone();
        drop(state);
        let signing_key = self
            .identity
            .get()
            .ok_or_else(|| anyhow!("authority requires a node identity"))?
            .signing_key()
            .clone();
        let store = store.ok_or_else(|| anyhow!("authority requires persistent storage"))?;
        drop(writer);
        Reporter::new(self.clone(), store, signing_key, authority).map(Some)
    }

    fn descriptor_snapshot(&self, timestamp: i64) -> Result<DescriptorSnapshot> {
        let state = self.state.lock().map_err(|_| anyhow!("node state"))?;
        Ok(DescriptorSnapshot {
            services: state.settings.services,
            stun_urls: state.settings.stun_urls.clone(),
            zone: state.settings.zone.clone(),
            region: state.settings.region.clone(),
            capacity_bytes_per_second: state.settings.capacity_bytes_per_second,
            authority: state.settings.authority.clone(),
            relay_ready: state.settings.services.relay
                && !state.unavailable
                && !state.budgets.public_exhausted(&state.settings, timestamp),
        })
    }

    pub(crate) fn descriptor(&self, endpoint: &crate::Endpoint) -> Result<serde_json::Value> {
        let identity = self
            .identity
            .get()
            .ok_or_else(|| anyhow!("missing node identity"))?;
        let timestamp = super::now();
        let now = timestamp.unsigned_abs();
        // Snapshot settings together so signing and access checks can run outside the state lock.
        let snapshot = self.descriptor_snapshot(timestamp)?;
        let accepting = self.access.public_enabled();
        let login_page = self
            .console
            .get()
            .filter(|_| snapshot.services.relay && accepting)
            .and_then(|console| console.login_page(now));
        let descriptor = serde_json::json!({
            "version": 2,
            "node_id": identity.id(),
            "node_key": identity.key_b64(),
            "relay_endpoint": endpoint.as_str(),
            "services": snapshot.services,
            "stun": snapshot.stun_urls,
            "zone": snapshot.zone,
            "region": snapshot.region,
            "capacity_bytes_per_second": snapshot.capacity_bytes_per_second,
            "classes": ["owner", "standard", "lowest"],
            "issued_at": now,
            "expires_at": now + DESCRIPTOR_SECONDS,
            "relay_available": snapshot.relay_ready && accepting,
            "anonymous": accepting && self.access.anonymous(),
            "account_login_page": login_page,
            "console_url": self.console.get().map(crate::console::Console::origin),
            "authority": snapshot.authority,
        });
        let payload = serde_json::to_vec(&descriptor)?;
        let signed = trust::sign(DESCRIPTOR_DOMAIN, &payload, identity.signing_key());
        Ok(serde_json::json!({
            "version": 2,
            "relay_url": endpoint.relay_url(),
            "descriptor": URL_SAFE_NO_PAD.encode(&payload),
            "signature": URL_SAFE_NO_PAD.encode(&signed[payload.len()..]),
        }))
    }
}

#[cfg(test)]
#[path = "descriptor_tests.rs"]
mod tests;
