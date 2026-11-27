use super::{
    Discovery,
    book::MAX_PEERS,
    record::{Envelope, MAX_RECORD},
};
use anyhow::Result;
use std::time::Duration;
use windshare_relay_access::descriptor::Purpose;

impl Discovery {
    async fn restore(
        &self,
        store: crate::Database,
        own_key: Option<&str>,
        endpoint: &crate::Endpoint,
    ) -> Result<()> {
        let raw = tokio::task::spawn_blocking(move || {
            store.cached_peers(MAX_PEERS * (MAX_RECORD + 1) + 2)
        })
        .await??;
        if let Some(raw) = raw {
            let entries: Vec<Envelope> = serde_json::from_slice(&raw)?;
            for entry in entries.into_iter().take(MAX_PEERS) {
                let now = crate::node::now().unsigned_abs();
                if let Ok(record) = entry.verify(Purpose::Gossip, now)
                    && record.offers_community_service()
                    && let Some(probe) = self.prepare_restored_probe(record, own_key, endpoint, now)
                {
                    // A restart loses endpoint reachability. Reprobe before advertising cached records.
                    if self.sender.send(probe).await.is_err() {
                        break;
                    }
                }
            }
        }
        Ok(())
    }

    pub(super) async fn maintain_cache(
        &self,
        store: &crate::Database,
        node: &crate::node::Runtime,
        endpoint: &crate::Endpoint,
    ) {
        if let Err(error) = self
            .restore(store.clone(), node.public_key_b64(), endpoint)
            .await
        {
            tracing::debug!(event="discovery_cache_read_failed",%error);
        }
        loop {
            self.dirty.notified().await;
            let records = self
                .book
                .lock()
                .expect("peer book")
                .records(crate::node::now().unsigned_abs());
            let db = store.clone();
            let write = tokio::task::spawn_blocking(move || -> Result<()> {
                db.cache_peers(&serde_json::to_vec(&records)?)
            })
            .await;
            if let Err(error) = write.unwrap_or_else(|error| Err(error.into())) {
                tracing::warn!(event="discovery_cache_write_failed",%error);
            }
            tokio::time::sleep(Duration::from_mins(1)).await;
        }
    }
}
