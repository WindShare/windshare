use super::{Key, Reporter};
use anyhow::Result;
use std::collections::BTreeMap;
use windshare_relay_access::usage::{Batch, Load, MAX_REPORT_RECORDS, UsageRecord};

impl Reporter {
    pub(super) fn batch(
        &self,
        records: Vec<UsageRecord>,
        resident_accounts: Vec<[u8; 16]>,
        now: u64,
    ) -> Result<Batch> {
        let load = self.node.snapshot();
        Ok(Batch {
            id: super::super::identity::random()?,
            issuer: self.authority.issuer()?,
            node: self.identity.key().verifying_key().to_bytes(),
            created_at: now,
            records,
            resident_accounts,
            shares: Vec::new(),
            load: Load {
                active_sessions: load.active_sessions,
                wire_bytes: load.wire_bytes,
                forwarded_bytes: load.forwarded_bytes,
                admission_refusals: load.admission_refusals,
            },
        })
    }

    pub(super) async fn flush(&self, now: u64) -> Result<()> {
        let records = self.node.usage.drain();
        let result = self.persist(&records, now).await;
        if result.is_err() {
            self.node.usage.restore(records);
        }
        result
    }

    async fn persist(&self, records: &BTreeMap<Key, u64>, now: u64) -> Result<()> {
        let issuer = self.authority.issuer()?;
        let mut batches = Vec::new();
        let records: Vec<_> = records
            .iter()
            .filter(|((i, _, _), _)| *i == issuer)
            .map(|((_, account, day), bytes)| UsageRecord {
                account: *account,
                day: *day,
                bytes: *bytes,
            })
            .collect();
        for chunk in records.chunks(MAX_REPORT_RECORDS) {
            batches.push(self.batch(chunk.to_vec(), Vec::new(), now)?);
        }
        let store = self.store.clone();
        tokio::task::spawn_blocking(move || store.enqueue_usage(&batches)).await??;
        Ok(())
    }

    pub(super) async fn refresh(&self, now: u64) -> Result<()> {
        let issuer = self.authority.issuer()?;
        let accounts = self
            .node
            .access
            .resident_accounts()
            .into_iter()
            .filter_map(|(i, a)| (i == issuer).then_some(a))
            .collect();
        let mut batch = self.batch(Vec::new(), accounts, now)?;
        batch.shares = self.node.access.resident_shares(issuer);
        self.upload(batch, false).await
    }

    pub(super) async fn retry(&self) -> Result<()> {
        let store = self.store.clone();
        let issuer = self.authority.issuer()?;
        let batch = tokio::task::spawn_blocking(move || store.pending_usage(issuer)).await??;
        if let Some(batch) = batch {
            self.upload(batch, true).await?;
        }
        Ok(())
    }
}
