use super::super::{Identity, Runtime};
use crate::storage::Database;
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Serialize};
use std::time::Duration;
use windshare_relay_access::{
    Issuer, trust,
    usage::{Batch, MAX_REPORT_BYTES, REPORT_INTERVAL_SECONDS, SignedReply, USAGE_DOMAIN},
};

const RETRY_SECONDS: u64 = 10;
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
        let mut ticks = tokio::time::interval(Duration::from_secs(RETRY_SECONDS));
        ticks.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        let mut next_report = 0;
        loop {
            ticks.tick().await;
            let now = super::super::now().unsigned_abs();
            if now >= next_report {
                next_report = now.saturating_add(REPORT_INTERVAL_SECONDS);
                if let Err(error) = self.flush(now).await {
                    tracing::error!(event="usage_persist_failed", %error);
                }
                // A failed old batch never holds up today's zero-byte entitlement refresh.
                let results: [Result<()>; 2] = tokio::join!(self.refresh(now), self.retry()).into();
                for result in results {
                    if let Err(error) = result {
                        tracing::warn!(event="authority_usage_retry", %error);
                    }
                }
            } else if let Err(error) = self.retry().await {
                tracing::warn!(event="authority_usage_retry", %error);
            }
        }
    }

    pub(super) async fn upload(&self, batch: Batch, durable: bool) -> Result<()> {
        let signed = trust::sign(
            USAGE_DOMAIN,
            &serde_json::to_vec(&batch)?,
            &self.identity.key(),
        );
        let mut response = self
            .client
            .post(format!(
                "{}/v1/node/usage",
                self.authority.url.trim_end_matches('/')
            ))
            .header("content-type", "application/octet-stream")
            .body(signed)
            .send()
            .await?
            .error_for_status()?;
        let mut bytes = Vec::new();
        while let Some(chunk) = response.chunk().await? {
            ensure!(
                bytes.len() + chunk.len() <= MAX_REPORT_BYTES,
                "oversized authority response"
            );
            bytes.extend_from_slice(&chunk);
        }
        let signed: SignedReply = serde_json::from_slice(&bytes)?;
        let cache = self.node.access.reply(
            signed,
            batch.issuer,
            batch.node,
            batch.id,
            super::super::now().unsigned_abs(),
        )?;
        let store = self.store.clone();
        let id = batch.id;
        // Persist the rollback barriers before activation or acknowledgement.
        let cache = tokio::task::spawn_blocking(move || -> Result<_> {
            let cache = store.save_access(cache)?;
            if durable {
                store.acknowledge_usage(id)?;
            }
            Ok(cache)
        })
        .await??;
        self.node.access.apply_cache(cache)?;
        tracing::info!(event="usage_batch_acknowledged", batch=?batch.id, node=?batch.node,
            records=batch.records.len(), resident_accounts=batch.resident_accounts.len(), durable);
        Ok(())
    }

    pub async fn shutdown(&self) -> Result<()> {
        self.flush(super::super::now().unsigned_abs()).await
    }
}
