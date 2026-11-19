use super::super::{Identity, Runtime, access::AuthorityState};
use crate::storage::Database;
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Serialize};
use std::time::Duration;
use tokio::sync::watch;
use windshare_relay_access::{
    Issuer, trust,
    trust::Generation,
    usage::{
        AccountEntitlement, Batch, MAX_REPORT_BYTES, NodePolicy, REPORT_INTERVAL_SECONDS,
        RESPONSE_DOMAIN, Reply, ReportResult, SignedReply, USAGE_DOMAIN,
    },
};

pub(super) const RETRY_SECONDS: u64 = 10;
const HTTP_TIMEOUT: Duration = Duration::from_secs(8);

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Authority {
    pub url: String,
    pub issuer: String,
}
impl Authority {
    pub fn issuer(&self) -> Result<Issuer> {
        URL_SAFE_NO_PAD
            .decode(&self.issuer)?
            .try_into()
            .map_err(|_| anyhow!("authority issuer width"))
    }
    pub fn validate(&self) -> Result<()> {
        self.issuer()?;
        let url = url::Url::parse(&self.url)?;
        let loopback = url.host_str().is_some_and(|host| {
            host == "localhost"
                || host
                    .parse::<std::net::IpAddr>()
                    .is_ok_and(|ip| ip.is_loopback())
        });
        ensure!(
            url.scheme() == "https" || url.scheme() == "http" && loopback,
            "authority requires HTTPS"
        );
        ensure!(
            url.username().is_empty()
                && url.password().is_none()
                && url.query().is_none()
                && url.fragment().is_none()
                && !url
                    .host_str()
                    .is_some_and(|host| host.ends_with(".workers.dev")),
            "authority must use its public API host"
        );
        Ok(())
    }
}

pub struct Reporter {
    pub(super) node: Runtime,
    pub(super) store: Database,
    pub(super) identity: Identity,
    pub(super) authority: Authority,
    client: reqwest::Client,
}
impl Reporter {
    pub fn new(
        node: Runtime,
        store: Database,
        identity: Identity,
        authority: Authority,
    ) -> Result<Self> {
        authority.validate()?;
        let client = reqwest::Client::builder()
            .timeout(HTTP_TIMEOUT)
            .redirect(reqwest::redirect::Policy::none())
            .build()?;
        node.usage.configure(authority.issuer()?);
        Ok(Self {
            node,
            store,
            identity,
            authority,
            client,
        })
    }

    pub async fn run(&self) {
        let (versions, updates) = watch::channel(0);
        tokio::join!(self.run_reports(&versions), self.sync_restrictions(updates));
    }

    async fn run_reports(&self, versions: &watch::Sender<u64>) {
        let mut ticks = tokio::time::interval(Duration::from_secs(RETRY_SECONDS));
        ticks.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        let mut next_report = 0;
        loop {
            ticks.tick().await;
            let now = super::super::now().unsigned_abs();
            if now >= next_report {
                next_report = now.saturating_add(REPORT_INTERVAL_SECONDS);
                if let Err(error) = self.flush(super::super::now).await {
                    tracing::error!(event="usage_persist_failed", %error);
                }
                // A failed old batch never holds up today's zero-byte entitlement refresh.
                let (refresh, retry, prune) =
                    tokio::join!(self.refresh(versions), self.retry(), self.prune_quotas(now));
                for result in [refresh, retry] {
                    if let Err(error) = result {
                        tracing::warn!(event="authority_usage_retry", %error);
                    }
                }
                if let Err(error) = prune {
                    tracing::warn!(event="access_quota_prune_failed", %error);
                }
            } else if let Err(error) = self.retry().await {
                tracing::warn!(event="authority_usage_retry", %error);
            }
        }
    }

    pub(super) async fn report(
        &self,
        batch: &Batch,
    ) -> Result<(Generation, Reply<ReportResult>, bool)> {
        let (signed, login) = self.post("/v1/node/usage", USAGE_DOMAIN, batch).await?;
        let (generation, reply) = self.node.access.verify_reply(
            signed,
            batch.issuer,
            batch.node,
            batch.id,
            super::super::now().unsigned_abs(),
            RESPONSE_DOMAIN,
        )?;
        Ok((generation, reply, login))
    }

    pub(super) async fn post(
        &self,
        path: &str,
        domain: &[u8],
        request: &(impl Serialize + Sync),
    ) -> Result<(SignedReply, bool)> {
        let signed = trust::sign(domain, &serde_json::to_vec(request)?, &self.identity.key());
        ensure!(signed.len() <= MAX_REPORT_BYTES, "oversized node request");
        let mut response = self
            .client
            .post(format!(
                "{}{path}",
                self.authority.url.trim_end_matches('/')
            ))
            .header("content-type", "application/octet-stream")
            .body(signed)
            .send()
            .await?
            .error_for_status()?;
        let login = response
            .headers()
            .get(windshare_relay_access::device_login::CAPABILITY_HEADER)
            .is_some_and(|header| {
                header == windshare_relay_access::device_login::CAPABILITY_VERSION
            });
        let mut bytes = Vec::new();
        while let Some(chunk) = response.chunk().await? {
            ensure!(
                bytes.len() + chunk.len() <= MAX_REPORT_BYTES,
                "oversized authority response"
            );
            bytes.extend_from_slice(&chunk);
        }
        Ok((serde_json::from_slice(&bytes)?, login))
    }

    pub(super) async fn persist_generation(&self, generation: Generation) -> Result<()> {
        let store = self.store.clone();
        let node = self.node.clone();
        tokio::task::spawn_blocking(move || {
            let writer = node.lock_control()?;
            // Persist the generation rollback barrier before the reply takes effect.
            let authority = store.save_authority(generation)?;
            let result = node.access.apply_authority(authority);
            drop(writer);
            result
        })
        .await?
    }

    pub(super) async fn persist_status(
        &self,
        generation: Generation,
        policy: NodePolicy,
        entitlements: Vec<AccountEntitlement>,
    ) -> Result<()> {
        let store = self.store.clone();
        let node = self.node.clone();
        let authority = AuthorityState {
            generation,
            policy: Some(policy),
        };
        tokio::task::spawn_blocking(move || {
            let writer = node.lock_control()?;
            let now = super::super::now().unsigned_abs();
            let update = store.save_status(authority, entitlements, now)?;
            let result = node.access.apply_access_update(update, now);
            drop(writer);
            result
        })
        .await?
    }

    async fn prune_quotas(&self, now: u64) -> Result<()> {
        let store = self.store.clone();
        tokio::task::spawn_blocking(move || store.prune_quotas(now)).await??;
        Ok(())
    }

    pub async fn shutdown(&self) -> Result<()> {
        self.flush(super::super::now).await
    }
}
