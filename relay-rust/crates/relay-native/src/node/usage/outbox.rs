use super::{Key, Reporter};
use anyhow::{Result, ensure};
use std::collections::BTreeMap;
use tokio::sync::watch;
use windshare_relay_access::usage::{
    Batch, Load, ReportResult, STATUS_BATCH_ENTRIES, USAGE_BATCH_ENTRIES, UsageRecord,
};

const _: () = assert!(crate::config::MAX_RESIDENT_SHARES <= STATUS_BATCH_ENTRIES);

impl Reporter {
    fn batch(
        &self,
        records: Vec<UsageRecord>,
        resident_accounts: Vec<[u8; 16]>,
        created_at: u64,
    ) -> Result<Batch> {
        let load = self.node.snapshot();
        let issuer = self.authority.issuer()?;
        let (applied_policy_version, resident_shares) = self
            .node
            .access
            .reported_policy(issuer, self.store.resident_inventory());
        Ok(Batch {
            id: super::super::identity::random()?,
            issuer,
            node: self.signing_key.verifying_key().to_bytes(),
            created_at,
            records,
            resident_accounts,
            shares: Vec::new(),
            load: Load {
                applied_policy_version,
                resident_shares,
                relay_budget: self
                    .node
                    .state
                    .lock()
                    .map_err(|_| anyhow::anyhow!("node state"))?
                    .settings
                    .relay_budget,
                active_sessions: load.active_sessions,
                wire_bytes: load.wire_bytes,
                forwarded_bytes: load.forwarded_bytes,
                admission_refusals: load.admission_refusals,
            },
        })
    }

    pub(super) async fn flush(&self, clock: impl FnOnce() -> i64) -> Result<()> {
        let records = self.node.usage.drain();
        // The scheduling tick can precede a concurrent write across UTC midnight.
        let created_at = clock().unsigned_abs();
        let result = self.persist(&records, created_at).await;
        if result.is_err() {
            self.node.usage.restore(records);
        }
        result
    }

    async fn persist(&self, records: &BTreeMap<Key, u64>, created_at: u64) -> Result<()> {
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
        for chunk in records.chunks(USAGE_BATCH_ENTRIES) {
            batches.push(self.batch(chunk.to_vec(), Vec::new(), created_at)?);
        }
        let store = self.store.clone();
        tokio::task::spawn_blocking(move || store.enqueue_usage(&batches)).await??;
        Ok(())
    }

    pub(super) async fn refresh(&self, versions: &watch::Sender<u64>) -> Result<()> {
        let issuer = self.authority.issuer()?;
        let accounts = self
            .node
            .access
            .resident_accounts()
            .into_iter()
            .filter_map(|(i, a)| (i == issuer).then_some(a))
            .collect();
        let mut batch = self.batch(Vec::new(), accounts, super::super::now().unsigned_abs())?;
        batch.shares = self.node.access.resident_shares(issuer);
        let (generation, reply, login) = self.report(&batch).await?;
        let ReportResult::Status {
            entitlements,
            restriction_version,
            policy,
        } = reply.result
        else {
            anyhow::bail!("expected node status");
        };
        ensure!(
            policy.version > 0
                && policy.probe_account == windshare_relay_access::probe::PROBE_ACCOUNT
                && entitlements.len() <= STATUS_BATCH_ENTRIES
                && entitlements.iter().all(|entry| {
                    entry.entitlement.valid_snapshot(reply.issued_at)
                        && batch
                            .resident_accounts
                            .binary_search(&entry.account)
                            .is_ok()
                }),
            "invalid node status"
        );
        let entitlement_count = entitlements.len();
        self.persist_status(generation, policy, entitlements)
            .await?;
        versions.send_if_modified(|current| {
            if restriction_version <= *current {
                return false;
            }
            *current = restriction_version;
            true
        });
        self.node.access.observe_device_login(issuer, login);
        tracing::debug!(
            event = "authority_status_refreshed",
            node = ?batch.node,
            batch = ?batch.id,
            entitlements = entitlement_count,
            restriction_version,
            policy_version = policy.version
        );
        Ok(())
    }

    pub(super) async fn retry(&self) -> Result<()> {
        let store = self.store.clone();
        let issuer = self.authority.issuer()?;
        let node = self.signing_key.verifying_key().to_bytes();
        let batch =
            tokio::task::spawn_blocking(move || store.next_usage_batch(issuer, node)).await??;
        let Some(batch) = batch else {
            return Ok(());
        };
        let (generation, reply, _) = self.report(&batch).await?;
        ensure!(
            matches!(reply.result, ReportResult::UsageReceived),
            "expected usage receipt"
        );
        self.persist_generation(generation).await?;
        let store = self.store.clone();
        let id = batch.id;
        tokio::task::spawn_blocking(move || store.acknowledge_usage(id)).await??;
        tracing::info!(event="usage_batch_acknowledged", batch=?batch.id, node=?batch.node,
            records=batch.records.len());
        Ok(())
    }
}
