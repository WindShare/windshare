use crate::{
    Account, Authority, Environment, Fault, Node, Store,
    store::{key, load, save},
};
use anyhow::{Result, anyhow, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Serialize, de::DeserializeOwned};
use sha2::{Digest, Sha256};
use std::collections::BTreeSet;
use windshare_relay_access::{
    Issuer,
    restrictions::{self, PageRequest},
    trust,
    usage::{
        AccountEntitlement, Batch, MAX_REPORT_BYTES, NodePolicy, RESPONSE_DOMAIN, Reply,
        ReportResult, SignedReply, USAGE_DOMAIN,
    },
};

impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Refuses unauthenticated nodes, conflicting batch IDs, invalid records or storage failures.
    pub fn usage(&mut self, signed: &[u8]) -> Result<SignedReply> {
        let batch: Batch = parse_signed(signed)?;
        let now = self.environment.now();
        ensure!(
            batch.valid() && batch.created_at <= now + trust::CLOCK_SKEW,
            Fault::Invalid
        );
        let node = self.authenticate_node(batch.issuer, batch.node, USAGE_DOMAIN, signed)?;
        // Expected admission refusals commit limiter state in the host transaction.
        // Validate all referenced accounts before writing anything for this report.
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

        let result = if batch.records.is_empty() {
            self.remember_shares(&batch.shares)?;
            self.status(&batch, &node)?
        } else {
            self.charge_usage(&batch, signed)?;
            ReportResult::UsageReceived
        };
        self.record_load(&batch, now)?;
        self.sign_reply(RESPONSE_DOMAIN, batch.node, batch.id, now, result)
    }

    /// # Errors
    /// Requires a valid signature from a registered node and readable restrictions.
    pub fn restriction_page(&self, signed: &[u8]) -> Result<SignedReply> {
        let request: PageRequest = parse_signed(signed)?;
        ensure!(request.id != [0; 16], Fault::Invalid);
        self.authenticate_node(
            request.issuer,
            request.node,
            restrictions::REQUEST_DOMAIN,
            signed,
        )?;
        let result = self.restriction_page_at(request.version, request.after)?;
        self.sign_reply(
            restrictions::RESPONSE_DOMAIN,
            request.node,
            request.id,
            self.environment.now(),
            result,
        )
    }

    fn authenticate_node(
        &self,
        issuer: Issuer,
        node: [u8; 32],
        domain: &[u8],
        signed: &[u8],
    ) -> Result<Node> {
        ensure!(issuer == self.signing.generation.issuer, Fault::Invalid);
        let node =
            load::<Node>(&self.store, &key("node", &node))?.ok_or_else(|| Fault::Denied {
                reason: "node_unregistered".into(),
            })?;
        trust::verify(domain, signed, &node.key).map_err(|_| Fault::Denied {
            reason: "node_signature".into(),
        })?;
        Ok(node)
    }

    fn sign_reply(
        &self,
        domain: &[u8],
        node: [u8; 32],
        request: [u8; 16],
        now: u64,
        result: impl Serialize,
    ) -> Result<SignedReply> {
        let reply = Reply {
            request,
            node,
            issuer: self.signing.generation.issuer,
            generation: self.signing.generation.number,
            issued_at: now,
            result,
        };
        let response = SignedReply {
            certificate: self.signing.certificate.clone(),
            body: URL_SAFE_NO_PAD.encode(trust::sign(
                domain,
                &serde_json::to_vec(&reply)?,
                &self.signing.key,
            )),
        };
        // Certificate and signed body are base64url, so the JSON envelope adds only fixed overhead.
        let bytes = response.body.len()
            + response.certificate.len()
            + br#"{"certificate":"","body":""}"#.len();
        ensure!(
            bytes <= MAX_REPORT_BYTES,
            "oversized signed authority response"
        );
        Ok(response)
    }

    fn charge_usage(&mut self, batch: &Batch, signed: &[u8]) -> Result<()> {
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
        if previous.is_none() {
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
            // Keep batch receipts so delayed retries cannot charge twice.
            save(&mut self.store, &receipt, &digest)?;
        }
        tracing::info!(event="node_usage_applied",node=?batch.node,batch=?batch.id,records=batch.records.len(),
            duplicate=previous.is_some());
        Ok(())
    }

    fn status(&mut self, batch: &Batch, node: &Node) -> Result<ReportResult> {
        let accounts: BTreeSet<_> = batch.resident_accounts.iter().copied().collect();
        let mut entitlements = Vec::with_capacity(accounts.len());
        for account in accounts {
            if load::<Account>(&self.store, &key("account", &account))?.is_some() {
                entitlements.push(AccountEntitlement {
                    account,
                    entitlement: self.entitlement(account)?,
                });
            }
        }
        let restriction_version = self.restriction_version()?;
        tracing::debug!(
            event = "node_status_served",
            node = ?batch.node,
            batch = ?batch.id,
            entitlements = entitlements.len(),
            restriction_version,
            policy_version = node.version,
        );
        Ok(ReportResult::Status {
            entitlements,
            restriction_version,
            policy: NodePolicy {
                version: node.version,
                enabled: node.enabled,
                anonymous: node.anonymous,
            },
        })
    }

    fn record_load(&mut self, batch: &Batch, now: u64) -> Result<()> {
        save(&mut self.store, &key("node-report", &batch.node), &now)?;
        let load_key = key("node-load", &batch.node);
        let previous_load =
            load::<(u64, windshare_relay_access::usage::Load)>(&self.store, &load_key)?;
        if previous_load.is_none_or(|(time, _)| batch.created_at >= time) {
            save(&mut self.store, &load_key, &(batch.created_at, &batch.load))?;
        }
        Ok(())
    }
}

fn parse_signed<T: DeserializeOwned>(signed: &[u8]) -> Result<T> {
    const SIGNATURE_BYTES: usize = 64;
    ensure!(
        (SIGNATURE_BYTES..=MAX_REPORT_BYTES).contains(&signed.len()),
        Fault::Invalid
    );
    Ok(serde_json::from_slice(
        &signed[..signed.len() - SIGNATURE_BYTES],
    )?)
}
