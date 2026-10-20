use super::{
    AccessObject, Credential, Database, LEASE_PREFIX, Lease, LeaseState, credentials, io,
    lease_key, provider, reply, save, trace,
};
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
        let Some(mut lease) = db.get::<Lease>(&lease_key(&request.lease_id))? else {
            return reply(&json!({"state":"expired"}), 200);
        };
        if !lease.owned_by(&request.subject(credential)) {
            return reply(&json!({"error":"access_denied"}), 403);
        }
        lease.withdraw(now);
        save(db, &lease)?;
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
        for mut lease in db.list::<Lease>(LEASE_PREFIX)? {
            if device.is_none_or(|id| id == lease.subject.device) {
                lease.withdraw(now);
                save(db, &lease)?;
                trace("turn_authority_withdrawn", &credentials::context(&lease));
            }
        }
        Ok(())
    }
    pub(in crate::platform::access) fn next_turn_deadline(db: &Database) -> Result<Option<u64>> {
        Ok(db
            .list::<Lease>(LEASE_PREFIX)?
            .iter()
            .filter_map(|lease| match lease.state {
                LeaseState::Issuing { deadline } | LeaseState::Withdrawing { deadline } => {
                    Some(deadline)
                }
                LeaseState::Active { .. } => Some(lease.expires_at),
                LeaseState::RevocationPending { next_attempt, .. } => {
                    Some(next_attempt.min(lease.expires_at))
                }
                LeaseState::Revoked | LeaseState::IssuanceFailed | LeaseState::Expired => None,
            })
            .min())
    }
    pub(in crate::platform::access) fn has_turn_work(db: &Database) -> Result<bool> {
        Ok(db.list::<Lease>(LEASE_PREFIX)?.iter().any(Lease::live))
    }
    pub(in crate::platform::access) async fn revoke_turn_leases(
        &self,
        db: &Database,
    ) -> Result<()> {
        let mut remaining = REVOCATION_BATCH;
        for mut lease in db.list::<Lease>(LEASE_PREFIX)? {
            let now = io::now().0;
            lease.settle_expiry(now);
            // Missing credentials include interrupted enrollment cleanup.
            if !self.turn_authorized(db, &lease.subject.device)? {
                lease.withdraw(now);
            }
            let due = matches!(lease.state, LeaseState::RevocationPending { next_attempt, .. } if next_attempt <= now);
            if !due || remaining == 0 {
                save(db, &lease)?;
                continue;
            }
            remaining -= 1;
            let LeaseState::RevocationPending {
                grant,
                next_attempt,
            } = &mut lease.state
            else {
                continue;
            };
            *next_attempt = now.saturating_add(REVOCATION_RETRY_MS);
            let username = grant.username.clone();
            save(db, &lease)?;
            // Rearm before external I/O, including on a failed alarm invocation.
            self.schedule().await?;
            let outcome = async {
                let secret = provider::secret(&self.env)?;
                if secret.key_id != lease.key_id {
                    return Err(super::failure("TURN key changed before revocation"));
                }
                provider::revoke(&secret, &username).await
            }
            .await;
            match outcome {
                Ok(()) => {
                    lease.state = LeaseState::Revoked;
                    save(db, &lease)?;
                    trace("turn_revocation_confirmed", &credentials::context(&lease));
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
