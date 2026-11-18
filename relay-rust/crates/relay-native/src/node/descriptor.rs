use super::{Identity, Runtime, Settings, usage::Reporter};
use anyhow::{Result, anyhow};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use windshare_relay_access::trust::{self, Generation};

const DESCRIPTOR_DOMAIN: &[u8] = b"windshare/node-descriptor/v2\0";
const DESCRIPTOR_SECONDS: u64 = 300;

impl Runtime {
    /// # Errors
    /// The identity must be installed before ingress starts.
    pub fn identify(&self, identity: Identity) -> Result<()> {
        self.state
            .lock()
            .map_err(|_| anyhow!("node state"))?
            .identity = Some(identity);
        Ok(())
    }

    pub(crate) fn open_console(
        &self,
        endpoint: &crate::Endpoint,
        store: &crate::Database,
    ) -> Result<()> {
        let identity = self
            .state
            .lock()
            .map_err(|_| anyhow!("node state"))?
            .identity
            .clone();
        if let Some(identity) = identity
            && let Some(console) =
                crate::console::Console::open(endpoint, store, self.access.clone(), &identity)?
        {
            self.console
                .set(console)
                .map_err(|_| anyhow!("console already initialized"))?;
        }
        Ok(())
    }

    pub(super) fn configure_access(&self, settings: &Settings) -> Result<()> {
        let state = self.state.lock().map_err(|_| anyhow!("node state"))?;
        let mut policy = settings.access.clone();
        if let Some(identity) = &state.identity {
            let owner = identity.owner();
            policy.owner_principals.push(owner.principal);
            policy.credentials.push(owner);
        }
        let store = state.control.clone();
        drop(state);
        self.access.set_controller(
            settings
                .authority
                .as_ref()
                .map(super::usage::Authority::issuer)
                .transpose()?,
        );
        if self.access.policy_matches(&policy) {
            return Ok(());
        }
        if let Some(store) = store {
            let mut cache = super::access::Cache::default();
            for issuer in &policy.trusted_issuers {
                let root = URL_SAFE_NO_PAD
                    .decode(&issuer.root)?
                    .try_into()
                    .map_err(|_| anyhow!("issuer width"))?;
                let generation =
                    Generation::verify(&URL_SAFE_NO_PAD.decode(&issuer.certificate)?, root)
                        .map_err(|error| anyhow!("issuer certificate: {error:?}"))?;
                cache.generations.push(generation);
            }
            // SQLite merges monotonically, so an old bootstrap certificate cannot
            // undo a generation learned from an authenticated authority response.
            let cache = store.save_access(cache)?;
            self.access.apply_cache(cache)?;
        }
        self.access.configure(policy)
    }

    pub(crate) fn reporter(&self) -> Result<Option<Reporter>> {
        let state = self.state.lock().map_err(|_| anyhow!("node state"))?;
        self.reporting_authority
            .get_or_init(|| state.settings.authority.clone());
        let Some(authority) = state.settings.authority.clone() else {
            return Ok(None);
        };
        let identity = state
            .identity
            .clone()
            .ok_or_else(|| anyhow!("authority requires a node identity"))?;
        let store = state
            .control
            .clone()
            .ok_or_else(|| anyhow!("authority requires persistent storage"))?;
        drop(state);
        Reporter::new(self.clone(), store, identity, authority).map(Some)
    }

    pub(crate) fn descriptor(&self, endpoint: &crate::Endpoint) -> Result<serde_json::Value> {
        let state = self.state.lock().map_err(|_| anyhow!("node state"))?;
        let identity = state
            .identity
            .as_ref()
            .ok_or_else(|| anyhow!("missing node identity"))?;
        let now = super::now().unsigned_abs();
        let descriptor = serde_json::json!({
            "version":2, "node_id":identity.id(), "node_key":URL_SAFE_NO_PAD.encode(identity.key().verifying_key().to_bytes()),
            "relay_endpoint":endpoint.as_str(), "services":state.settings.services,
            "stun":state.settings.stun_urls, "zone":state.settings.zone, "region":state.settings.region,
            "capacity_bytes_per_second":state.settings.capacity_bytes_per_second,
            "classes":["owner","standard","lowest"], "issued_at":now, "expires_at":now+DESCRIPTOR_SECONDS,
            "relay_available":!state.unavailable && self.access.public_enabled(), "anonymous":self.access.anonymous(),
            "console_url": self.console.get().map(crate::console::Console::origin), "authority":state.settings.authority
        });
        let payload = serde_json::to_vec(&descriptor)?;
        let signed = trust::sign(DESCRIPTOR_DOMAIN, &payload, &identity.key());
        drop(state);
        Ok(
            serde_json::json!({"version":2,"relay_url":endpoint.relay_url(),"descriptor":URL_SAFE_NO_PAD.encode(&payload),
            "signature":URL_SAFE_NO_PAD.encode(&signed[payload.len()..])}),
        )
    }
}
