use crate::{
    Account, Authority, Environment, Fault, Node, Store,
    store::{key, load, save},
};
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use sha2::{Digest, Sha256};
use std::collections::BTreeSet;
use windshare_relay_access::{
    trust,
    usage::{
        AccountEntitlement, Batch, MAX_REPORT_BYTES, RESPONSE_DOMAIN, Reply, SignedReply,
        USAGE_DOMAIN,
    },
};

impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Refuses unauthenticated nodes, conflicting batch IDs, invalid records or storage failures.
    pub fn usage(&mut self, signed: &[u8]) -> Result<SignedReply> {
        ensure!(
            (64..=MAX_REPORT_BYTES).contains(&signed.len()),
            Fault::Invalid
        );
        let batch: Batch = serde_json::from_slice(&signed[..signed.len() - 64])?;
        ensure!(
            batch.valid()
                && batch.issuer == self.signing.generation.issuer
                && batch.created_at <= self.environment.now() + trust::CLOCK_SKEW,
            Fault::Invalid
        );
        let node =
            load::<Node>(&self.store, &key("node", &batch.node))?.ok_or_else(|| Fault::Denied {
                reason: "node_unregistered".into(),
            })?;
        trust::verify(USAGE_DOMAIN, signed, &node.key).map_err(|_| Fault::Denied {
            reason: "node_signature".into(),
        })?;
        // Validate the complete batch before any charge: expected admission
        // refusals commit limiter state in the host transaction.
        for account in batch
            .records
            .iter()
            .map(|record| record.account)
            .chain(batch.shares.iter().map(|entry| entry.account))
        {
            ensure!(
                load::<Account>(&self.store, &key("account", &account))?.is_some(),
                Fault::Invalid
            );
        }
        let receipt = format!(
            "{}/{}",
            key("batch", &batch.node),
            URL_SAFE_NO_PAD.encode(batch.id)
        );
        let digest: [u8; 32] = Sha256::digest(signed).into();
        let previous = load::<[u8; 32]>(&self.store, &receipt)?;
        ensure!(
            previous.is_none_or(|previous| previous == digest),
            Fault::Invalid
        );
        // Empty refreshes do not need a permanent deduplication record. Byte batches
        // keep their IDs indefinitely so delayed retries cannot charge twice.
        if previous.is_none() && !batch.records.is_empty() {
            for record in &batch.records {
                let usage_key = format!("{}/{}", key("usage", &record.account), record.day);
                let used = load::<u64>(&self.store, &usage_key)?.unwrap_or(0);
                save(
                    &mut self.store,
                    &usage_key,
                    &used
                        .checked_add(record.bytes)
                        .ok_or_else(|| anyhow!("usage overflow"))?,
                )?;
            }
            save(&mut self.store, &receipt, &digest)?;
        }
        self.remember_shares(&batch.shares)?;
        self.record_load(&batch)?;
        let accounts: BTreeSet<_> = batch
            .resident_accounts
            .iter()
            .copied()
            .chain(batch.records.iter().map(|record| record.account))
            .collect();
        let mut entitlements = Vec::new();
        for account in accounts {
            if load::<Account>(&self.store, &key("account", &account))?.is_some() {
                entitlements.push(AccountEntitlement {
                    account,
                    entitlement: self.entitlement(account)?,
                });
            }
        }
        let reply = Reply {
            batch: batch.id,
            node: batch.node,
            issuer: batch.issuer,
            generation: self.signing.generation.number,
            issued_at: self.environment.now(),
            entitlements,
            restrictions: self.restrictions()?,
            policy: windshare_relay_access::usage::NodePolicy {
                version: node.version,
                enabled: node.enabled,
                anonymous: node.anonymous,
            },
        };
        tracing::info!(event="node_usage_applied",node=?batch.node,batch=?batch.id,records=batch.records.len(),
            duplicate=previous.is_some(),restriction_version=reply.restrictions.version);
        Ok(SignedReply {
            certificate: self.signing.certificate.clone(),
            body: URL_SAFE_NO_PAD.encode(trust::sign(
                RESPONSE_DOMAIN,
                &serde_json::to_vec(&reply)?,
                &self.signing.key,
            )),
        })
    }

    fn record_load(&mut self, batch: &Batch) -> Result<()> {
        save(
            &mut self.store,
            &key("node-report", &batch.node),
            &self.environment.now(),
        )?;
        let load_key = key("node-load", &batch.node);
        let previous_load =
            load::<(u64, windshare_relay_access::usage::Load)>(&self.store, &load_key)?;
        if previous_load.is_none_or(|(time, _)| batch.created_at >= time) {
            save(&mut self.store, &load_key, &(batch.created_at, &batch.load))?;
        }
        Ok(())
    }
}
