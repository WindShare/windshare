use super::{Reporter, reporting::RETRY_SECONDS};
use crate::node::access::Cache;
use anyhow::{Result, ensure};
use std::time::Duration;
use tokio::sync::watch;
use windshare_relay_access::{
    Issuer,
    restrictions::{self, PAGE_ENTRIES, PageRequest, PageResult, Restriction, Restrictions},
    trust::Generation,
};

impl Reporter {
    pub(super) async fn sync_restrictions(&self, mut versions: watch::Receiver<u64>) {
        let issuer = self.authority.issuer().expect("validated authority issuer");
        let mut target = self.node.access.restriction_version(issuer);
        loop {
            if target <= self.node.access.restriction_version(issuer) {
                let installed = self.node.access.restriction_version(issuer);
                let Ok(version) = versions.wait_for(|version| *version > installed).await else {
                    return;
                };
                target = *version;
            }
            target = target.max(*versions.borrow_and_update());
            match self.sync_version(issuer, target).await {
                Ok(Some(current)) => target = current,
                Ok(None) => {}
                Err(error) => {
                    tracing::warn!(event="restriction_sync_failed", ?issuer, version=target, %error);
                    tokio::time::sleep(Duration::from_secs(RETRY_SECONDS)).await;
                }
            }
        }
    }

    async fn sync_version(&self, issuer: Issuer, version: u64) -> Result<Option<u64>> {
        let mut snapshot = Restrictions {
            version,
            ..Restrictions::default()
        };
        let mut after = None;
        loop {
            let (generation, result) = self.retry_page(issuer, version, after).await;
            match result {
                PageResult::Superseded { version: current } => {
                    ensure!(current > version, "authority restriction version rollback");
                    return Ok(Some(current));
                }
                PageResult::Page { entries, more } => {
                    ensure!(
                        entries.len() <= PAGE_ENTRIES && (!more || entries.len() == PAGE_ENTRIES),
                        "invalid authority restriction page"
                    );
                    after = entries.last().copied();
                    for entry in entries {
                        ensure!(snapshot.insert(entry), "duplicate authority restriction");
                    }
                    if !more {
                        self.install_restrictions(issuer, generation, snapshot)
                            .await?;
                        return Ok(None);
                    }
                }
            }
        }
    }

    async fn retry_page(
        &self,
        issuer: Issuer,
        version: u64,
        after: Option<Restriction>,
    ) -> (Generation, PageResult) {
        loop {
            match self.restriction_page(issuer, version, after).await {
                Ok(page) => return page,
                Err(error) => {
                    tracing::warn!(event="restriction_page_retry", ?issuer, version, ?after, %error);
                    tokio::time::sleep(Duration::from_secs(RETRY_SECONDS)).await;
                }
            }
        }
    }

    async fn restriction_page(
        &self,
        issuer: Issuer,
        version: u64,
        after: Option<Restriction>,
    ) -> Result<(Generation, PageResult)> {
        let request = PageRequest {
            id: super::super::identity::random()?,
            issuer,
            node: self.identity.key().verifying_key().to_bytes(),
            version,
            after,
        };
        let (signed, _) = self
            .post(
                "/v1/node/restrictions",
                restrictions::REQUEST_DOMAIN,
                &request,
            )
            .await?;
        let (generation, reply) = self.node.access.verify_reply(
            signed,
            issuer,
            request.node,
            request.id,
            super::super::now().unsigned_abs(),
            restrictions::RESPONSE_DOMAIN,
        )?;
        Ok((generation, reply.result))
    }

    async fn install_restrictions(
        &self,
        issuer: Issuer,
        generation: Generation,
        snapshot: Restrictions,
    ) -> Result<()> {
        self.persist_access(Cache {
            generations: vec![generation],
            ..Cache::default()
        })
        .await?;
        let store = self.store.clone();
        let snapshot = tokio::task::spawn_blocking(move || {
            store.save_restrictions(issuer, &snapshot)?;
            Ok::<_, anyhow::Error>(snapshot)
        })
        .await??;
        self.node.access.restrict(issuer, snapshot);
        Ok(())
    }
}
