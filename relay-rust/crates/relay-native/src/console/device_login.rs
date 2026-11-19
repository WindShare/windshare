use super::{Console, Reply, Result, database, field, refusal};
use rusqlite::{Connection, params};
use serde_json::{Value, json};
use windshare_relay_access::{
    Enrollment, EnrollmentState, Refusal,
    device_login::{Approval, Invitation},
    trust::Generation,
};

impl Console {
    fn login_authority(&self, now: u64) -> Option<(&str, Generation)> {
        let service = self.0.account_service.as_ref()?;
        let generation = self
            .0
            .access
            .login_generation(service.issuer().ok()?, now)?;
        Some((&service.url, generation))
    }
    pub(crate) fn login_page(&self, now: u64) -> Option<String> {
        self.login_authority(now)
            .map(|(url, _)| format!("{}/device", url.trim_end_matches('/')))
    }
    pub(super) fn account_invitation(
        &self,
        enrollment: &Enrollment,
        now_ms: u64,
    ) -> Result<Option<String>> {
        let Some((_, generation)) = self.login_authority(now_ms / 1000) else {
            return Ok(None);
        };
        let invite = Invitation {
            node: self.0.node_key.verifying_key().to_bytes(),
            issuer: generation.issuer,
            enrollment: enrollment.id,
            origin: self.origin().to_owned(),
            device: enrollment.device_name.clone(),
            code: enrollment.user_code.clone(),
            expires_ms: enrollment.expires_at,
        };
        Ok(Some(invite.issue(&self.0.node_key).map_err(|e| {
            anyhow::anyhow!("device invitation: {e:?}")
        })?))
    }
    pub(super) fn approve_account(&self, db: &Connection, body: &Value, now: u64) -> Result<Reply> {
        let denied = || refusal(Refusal::AccessDenied);
        let (_, generation) = self.login_authority(now / 1000).ok_or_else(denied)?;
        let grant = Approval::verify(
            field(body, "approval")?,
            generation,
            self.0.node_key.verifying_key().to_bytes(),
            now,
        )
        .map_err(|_| denied())?;
        let mut e = database::enrollment(db, &grant.enrollment)?
            .ok_or_else(|| refusal(Refusal::Expired))?;
        let binding = crate::node::access::BoundAccount {
            issuer: grant.issuer,
            account: grant.account,
        };
        if e.state == EnrollmentState::Pending {
            super::devices::require_credential_capacity(db)?;
            e.approve(e.id, now).map_err(refusal)?;
            db.execute(
                "INSERT INTO console_account_bindings(credential,issuer,account) VALUES(?1,?2,?3)",
                params![
                    e.id.as_slice(),
                    grant.issuer.as_slice(),
                    grant.account.as_slice()
                ],
            )?;
            database::save_enrollment(db, &e)?;
            tracing::info!(event="service_device_bound",enrollment=?e.id,account=?grant.account);
        } else {
            let existing = match e.state {
                EnrollmentState::Approved { credential }
                | EnrollmentState::Claimed { credential }
                | EnrollmentState::Installed { credential } => Some(credential),
                _ => None,
            };
            if existing != Some(e.id) || database::binding(db, &e.id)? != Some(binding) {
                return Err(denied().into());
            }
        }
        self.0
            .access
            .refresh(grant.issuer, grant.account, grant.entitlement, now / 1000);
        Ok(Reply::ok(json!({"ok":true})))
    }
}
