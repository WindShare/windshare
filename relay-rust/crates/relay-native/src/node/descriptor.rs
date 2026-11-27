use super::{Identity, Runtime, Services, usage::Reporter};
use anyhow::{Result, anyhow};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use ed25519_dalek::SigningKey;
use windshare_relay_access::{Credential, trust};

use windshare_relay_access::descriptor::{PublicRelay, Purpose, offers_community_service};

pub enum GossipDescriptor {
    /// Owner explicitly disabled community contribution on this self-hosted node.
    OptedOut,
    /// Node does not currently advertise a community relay or STUN service
    /// (for example an official seed or temporarily exhausted relay without STUN).
    ObserverOnly,
    /// Signed gossip descriptor for a contributing community node.
    Advertise(serde_json::Value),
}

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
    public_relay: PublicRelay,
    operator: String,
    provider: String,
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

    pub(crate) fn report_stun_ports(&self, ports: impl Iterator<Item = u16>) -> Result<()> {
        let mut ports: Vec<_> = ports.collect();
        ports.sort_unstable();
        ports.dedup();
        self.state
            .lock()
            .map_err(|_| anyhow!("node state"))?
            .bound_stun_ports = Some(ports);
        Ok(())
    }

    fn descriptor_snapshot(
        &self,
        timestamp: i64,
        endpoint: &crate::Endpoint,
    ) -> Result<DescriptorSnapshot> {
        let state = self.state.lock().map_err(|_| anyhow!("node state"))?;
        let mut services = state.settings.services;
        // The kernel may select an ephemeral port or refuse one address family.
        // Only advertise STUN after a listener exists, honoring explicit NAT port mappings.
        let stun_urls =
            if !services.stun || state.bound_stun_ports.as_ref().is_some_and(Vec::is_empty) {
                Vec::new()
            } else if !state.settings.stun_urls.is_empty() {
                state.settings.stun_urls.clone()
            } else {
                let url = url::Url::parse(endpoint.as_str())?;
                let host = url.host_str().ok_or_else(|| anyhow!("missing STUN host"))?;
                state
                    .bound_stun_ports
                    .iter()
                    .flatten()
                    .take(windshare_relay_access::descriptor::MAX_STUN_URLS)
                    .map(|port| format!("stun:{host}:{port}"))
                    .collect()
            };
        services.stun = !stun_urls.is_empty();
        Ok(DescriptorSnapshot {
            services,
            stun_urls,
            zone: state.settings.zone.clone(),
            region: state.settings.region.clone(),
            capacity_bytes_per_second: state
                .settings
                .contribution
                .bytes_per_second
                .or(state.settings.ceiling.bytes_per_second),
            operator: state.settings.operator.clone(),
            provider: state.settings.provider.clone(),
            authority: state.settings.authority.clone(),
            public_relay: if !state.settings.access.contributing {
                PublicRelay::Disabled
            } else if state.storage_failed
                || !state.settings.services.relay
                || !self.access.public_enabled()
            {
                PublicRelay::Unavailable
            } else if state.budgets.public_exhausted(&state.settings, timestamp)
                || state.budgets.node_remaining(&state.settings) == 0
            {
                PublicRelay::BudgetExhausted
            } else if state.unavailable {
                PublicRelay::Unavailable
            } else {
                PublicRelay::Enabled {
                    anonymous: self.access.anonymous(),
                }
            },
        })
    }

    pub(crate) fn descriptor(&self, endpoint: &crate::Endpoint) -> Result<serde_json::Value> {
        self.sign_descriptor(endpoint, Purpose::Live)
    }

    pub(crate) fn gossip_descriptor(&self, endpoint: &crate::Endpoint) -> Result<GossipDescriptor> {
        let timestamp = super::now();
        let now = timestamp.unsigned_abs();
        let snapshot = self.descriptor_snapshot(timestamp, endpoint)?;
        if snapshot.public_relay == PublicRelay::Disabled {
            return Ok(GossipDescriptor::OptedOut);
        }
        if !offers_community_service(
            snapshot.authority.is_some(),
            snapshot.services.relay,
            snapshot.services.stun,
            snapshot.public_relay,
        ) {
            return Ok(GossipDescriptor::ObserverOnly);
        }
        self.sign_snapshot(endpoint, Purpose::Gossip, now, &snapshot)
            .map(GossipDescriptor::Advertise)
    }

    pub(crate) fn sign_descriptor(
        &self,
        endpoint: &crate::Endpoint,
        purpose: Purpose,
    ) -> Result<serde_json::Value> {
        let timestamp = super::now();
        let now = timestamp.unsigned_abs();
        // Snapshot settings together so signing and access checks can run outside the state lock.
        let snapshot = self.descriptor_snapshot(timestamp, endpoint)?;
        self.sign_snapshot(endpoint, purpose, now, &snapshot)
    }

    fn sign_snapshot(
        &self,
        endpoint: &crate::Endpoint,
        purpose: Purpose,
        now: u64,
        snapshot: &DescriptorSnapshot,
    ) -> Result<serde_json::Value> {
        let identity = self
            .identity
            .get()
            .ok_or_else(|| anyhow!("missing node identity"))?;
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
            "public_capacity_bytes_per_second": snapshot.capacity_bytes_per_second,
            "operator": snapshot.operator,
            "provider": snapshot.provider,
            "platform": "native",
            "classes": ["owner", "standard", "lowest"],
            "issued_at": now,
            "expires_at": now + purpose.lifetime(),
            "public_relay": snapshot.public_relay,
            "account_login_page": login_page,
            "console_url": self.console.get().map(crate::console::Console::origin),
            "authority": snapshot.authority,
        });
        let payload = serde_json::to_vec(&descriptor)?;
        let signed = trust::sign(purpose.domain(), &payload, identity.signing_key());
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
