use crate::{
    Account, Authority, Environment, Fault, Node, NodeLoadReport, Store,
    accounts::DailyUsageRecord,
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

enum ReportingNode {
    Enrolled(NodePolicy),
    Retired(NodePolicy),
}

impl<R: Store, E: Environment> Authority<R, E> {
    /// # Errors
    /// Refuses unauthenticated nodes, conflicting batch IDs, invalid records or storage failures.
    pub fn usage(&mut self, signed: &[u8]) -> Result<SignedReply> {
        let batch: Batch = parse_signed(signed)?;
        let now = self.environment.now();
        ensure!(batch.valid(), Fault::Invalid);
        let usage = !batch.records.is_empty();
        let reporting = self.authenticate_node(batch.issuer, batch.node, USAGE_DOMAIN, signed)?;
        // Queued accounting batches can arrive after an outage, so only future
        // skew is bounded. Live status heartbeats must sit within the symmetric
        // clock-skew window so stale signed status batches cannot be replayed.
        let fresh = if usage {
            batch.created_at <= now.saturating_add(trust::CLOCK_SKEW)
        } else {
            batch.created_at.abs_diff(now) <= trust::CLOCK_SKEW
        };
        if !fresh {
            tracing::warn!(
                event = "node_report_clock_skew",
                node = ?batch.node,
                batch = ?batch.id,
                usage,
                created_at = batch.created_at,
                now,
                max_skew = trust::CLOCK_SKEW,
            );
            return Err(Fault::Invalid.into());
        }
        let policy = match reporting {
            ReportingNode::Enrolled(policy) => policy,
            ReportingNode::Retired(policy) => {
                // An offline node may first learn of an early cutoff after removal.
                // Give it the final policy without granting access to any user records.
                let reply = if usage {
                    ReportResult::UsageReceived
                } else {
                    ReportResult::Status {
                        entitlements: vec![],
                        restriction_version: 0,
                        policy,
                    }
                };
                return self.sign_reply(RESPONSE_DOMAIN, batch.node, batch.id, now, reply);
            }
        };
        // Expected admission refusals commit limiter state in the host transaction.
        // Validate all referenced accounts before writing anything for this report.
        for record in &batch.records {
            ensure!(
                load::<Account>(&self.store, &key("account", &record.account))?.is_some(),
                Fault::Invalid
            );
        }

        let result = if usage {
            self.charge_usage(&batch, signed)?;
            ReportResult::UsageReceived
        } else {
            let status = self.status(&batch, policy)?;
            // Queued accounting batches can outlive several policy revisions.
            // Only the signed status round trip can acknowledge a drain.
            self.record_load(&batch, now)?;
            status
        };
        self.sign_reply(RESPONSE_DOMAIN, batch.node, batch.id, now, result)
    }

    /// # Errors
    /// Requires a valid signature from a registered node and readable restrictions.
    pub fn restriction_page(&self, signed: &[u8]) -> Result<SignedReply> {
        let request: PageRequest = parse_signed(signed)?;
        ensure!(request.id != [0; 16], Fault::Invalid);
        ensure!(
            matches!(
                self.authenticate_node(
                    request.issuer,
                    request.node,
                    restrictions::REQUEST_DOMAIN,
                    signed,
                )?,
                ReportingNode::Enrolled(_)
            ),
            Fault::Denied {
                reason: "node_unregistered".into()
            }
        );
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
    ) -> Result<ReportingNode> {
        ensure!(issuer == self.signing.generation.issuer, Fault::Invalid);
        let reporter = if let Some(enrolled) = load::<Node>(&self.store, &key("node", &node))? {
            ReportingNode::Enrolled(enrolled.policy())
        } else {
            let policy =
                load(&self.store, &key("retired-node", &node))?.ok_or_else(|| Fault::Denied {
                    reason: "node_unregistered".into(),
                })?;
            ReportingNode::Retired(policy)
        };
        trust::verify(domain, signed, &node).map_err(|_| Fault::Denied {
            reason: "node_signature".into(),
        })?;
        Ok(reporter)
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
                let current =
                    load::<DailyUsageRecord>(&self.store, &usage_key)?.unwrap_or_default();
                let updated = DailyUsageRecord {
                    bytes: current
                        .bytes
                        .checked_add(record.bytes)
                        .ok_or_else(|| anyhow!("usage overflow"))?,
                    version: current
                        .version
                        .checked_add(1)
                        .ok_or_else(|| anyhow!("entitlement version exhausted"))?,
                };
                save(&mut self.store, &usage_key, &updated)?;
            }
            // Keep batch receipts so delayed retries cannot charge twice.
            save(&mut self.store, &receipt, &digest)?;
        }
        tracing::info!(event="node_usage_applied",node=?batch.node,batch=?batch.id,records=batch.records.len(),
            duplicate=previous.is_some());
        Ok(())
    }

    fn status(&self, batch: &Batch, policy: NodePolicy) -> Result<ReportResult> {
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
            policy_version = policy.version,
        );
        Ok(ReportResult::Status {
            entitlements,
            restriction_version,
            policy,
        })
    }

    fn record_load(&mut self, batch: &Batch, now: u64) -> Result<()> {
        let load_key = key("node-load", &batch.node);
        let previous = load::<NodeLoadReport>(&self.store, &load_key)?;
        // Only live status heartbeats reach this path; trust receipt order rather
        // than node wall clocks so backward NTP steps never hide drain progress,
        // and persist every heartbeat so Durable Object eviction never reverts
        // liveness or drain confirmation to a stale snapshot.
        let next = NodeLoadReport {
            created_at: batch.created_at,
            reported_at: previous.map_or(now, |entry| now.max(entry.reported_at)),
            load: batch.load.clone(),
        };
        save(&mut self.store, &load_key, &next)?;
        tracing::debug!(
            event = "node_load_recorded",
            node = ?batch.node,
            created_at = next.created_at,
            reported_at = next.reported_at
        );
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
