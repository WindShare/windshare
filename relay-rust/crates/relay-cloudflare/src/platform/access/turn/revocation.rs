use super::{AccessObject, Credential, Database, credentials, io, provider, reply, trace};
use serde_json::json;
use worker::{Response, Result};

const REVOCATION_BATCH: usize = 16;
const REVOCATION_RETRY_MS: u64 = 5_000;
impl AccessObject {
    pub(super) fn turn_revoke(
        db: &Database,
        credential: &Credential,
        request: &credentials::LeaseRequest,
        now: u64,
    ) -> Result<Response> {
        let subject = request.subject(credential);
        let Some((lease, owned)) = db.update_turn_lease(&request.lease_id, |lease| {
            let owned = lease.owned_by(&subject);
            if owned {
                lease.withdraw(now);
            }
            Ok(owned)
        })?
        else {
            return reply(&json!({"state":"expired"}), 200);
        };
        if !owned {
            return reply(&json!({"error":"access_denied"}), 403);
        }
        trace("turn_revocation_requested", &credentials::context(&lease));
        reply(
            &json!({"state":lease.status()}),
            if lease.pending() { 202 } else { 200 },
        )
    }
    pub(in crate::platform::access) fn withdraw_turn(
        db: &Database,
        device: Option<&str>,
        now: u64,
    ) -> Result<()> {
        for id in db.open_turn_leases(device)? {
            if let Some((lease, ())) = db.update_turn_lease(&id, |lease| {
                lease.withdraw(now);
                Ok(())
            })? {
                trace("turn_authority_withdrawn", &credentials::context(&lease));
            }
        }
        Ok(())
    }
    fn reconcile_turn_leases(&self, db: &Database, now: u64) -> Result<()> {
        for id in db.open_turn_leases(None)? {
            let Some((lease, authorized)) = db.update_turn_lease(&id, |lease| {
                lease.settle_expiry(now);
                // Missing credentials include interrupted enrollment cleanup.
                let authorized = self.turn_authorized(db, &lease.subject.device)?;
                if !authorized {
                    lease.withdraw(now);
                }
                Ok(authorized)
            })?
            else {
                continue;
            };
            if !authorized {
                trace("turn_authority_withdrawn", &credentials::context(&lease));
            }
        }
        Ok(())
    }
    pub(in crate::platform::access) async fn revoke_turn_leases(
        &self,
        db: &Database,
    ) -> Result<()> {
        self.reconcile_turn_leases(db, io::now().0)?;
        // A candidate list contains only identities. Each attempt reserves its retry
        // against current state, so another request cannot be undone after a yield.
        for id in db.due_turn_revocations(io::now().0, REVOCATION_BATCH)? {
            let Some((lease, Some(revocation))) = db.update_turn_lease(&id, |lease| {
                Ok(lease.begin_revocation(io::now().0, REVOCATION_RETRY_MS))
            })?
            else {
                continue;
            };
            // Rearm before external I/O, including on a failed alarm invocation.
            self.schedule().await?;
            let outcome = async {
                let secret = provider::secret(&self.env)?;
                if secret.key_id != revocation.key_id {
                    return Err(super::failure("TURN key changed before revocation"));
                }
                provider::revoke(&secret, &revocation.username).await
            }
            .await;
            match outcome {
                Ok(()) => {
                    if let Some((current, true)) = db
                        .update_turn_lease(&id, |lease| Ok(lease.confirm_revocation(&revocation)))?
                    {
                        trace("turn_revocation_confirmed", &credentials::context(&current));
                    } else {
                        trace(
                            "turn_revocation_result_obsolete",
                            &json!({"lease_id":id,"service_id":lease.service_id,
                                "key_id":revocation.key_id,"username":revocation.username}),
                        );
                    }
                }
                Err(error) => trace(
                    "turn_revocation_pending",
                    &json!({"service_id":lease.service_id,
                    "device_id":lease.subject.device,"session_id":lease.subject.session,"lease_id":lease.id,
                    "reason":error.to_string(),"retry_after_ms":REVOCATION_RETRY_MS}),
                ),
            }
        }
        Ok(())
    }
}
