use super::{AccessObject, Database, failure, key, random};
use windshare_relay_access::{Credential, Digest, Enrollment, EnrollmentState};
use windshare_relay_console::{
    access::{AccessError, AccessStore},
    credentials::RevokedCredentialPosition,
    http::ConsoleError,
    text_id,
};
use worker::Result;

const ENROLLMENT_ATTEMPTS: u32 = 30;
const GLOBAL_ENROLLMENT_ATTEMPTS: u32 = 1000;
const ENROLLMENT_RETRY_MS: u64 = 60_000;

pub(super) struct CloudflareAccessStore<'a> {
    db: &'a Database,
    source_ip: String,
}

impl<'a> CloudflareAccessStore<'a> {
    pub(super) const fn new(db: &'a Database, source_ip: String) -> Self {
        Self { db, source_ip }
    }

    pub(super) fn owner_principal(db: &Database) -> Result<[u8; 16]> {
        if let Some(principal) = db.get("principal:owner")? {
            return Ok(principal);
        }
        let principal: [u8; 16] = random()?;
        db.put("principal:owner", &principal, None)?;
        Ok(principal)
    }
}

impl AccessStore for CloudflareAccessStore<'_> {
    type Error = worker::Error;

    fn fill_random(&mut self, dest: &mut [u8]) -> Result<()> {
        getrandom::fill(dest).map_err(|error| failure(format!("Worker entropy: {error}")))
    }

    fn check_enrollment_rate_limit(
        &mut self,
        now: u64,
    ) -> std::result::Result<(), AccessError<worker::Error>> {
        let global_ok =
            AccessObject::rate_limit(self.db, "enroll:global", GLOBAL_ENROLLMENT_ATTEMPTS, now)
                .map_err(AccessError::Store)?;
        let source_scope = format!(
            "enroll:{}",
            text_id(&Digest::of(self.source_ip.as_bytes()).0)
        );
        let source_ok = AccessObject::rate_limit(self.db, &source_scope, ENROLLMENT_ATTEMPTS, now)
            .map_err(AccessError::Store)?;
        if !global_ok || !source_ok {
            return Err(ConsoleError::too_many_requests("slow_down", ENROLLMENT_RETRY_MS).into());
        }
        Ok(())
    }

    fn pending_enrollment_count(&self) -> Result<usize> {
        self.db.pending_enrollments()
    }

    fn enrollment_by_claimant(&self, claimant: Digest) -> Result<Option<Enrollment>> {
        let Some(id) = self.db.get::<[u8; 16]>(&key("request", &claimant.0))? else {
            return Ok(None);
        };
        self.db.get(&key("enrollment", &id))
    }

    fn enrollment(&self, id: &[u8; 16]) -> Result<Option<Enrollment>> {
        self.db.get(&key("enrollment", id))
    }

    fn list_pending_enrollments(&self) -> Result<Vec<Enrollment>> {
        self.db.list_pending_enrollments()
    }

    fn save_enrollment(&mut self, enrollment: &Enrollment) -> Result<()> {
        // Installed receipts stay durable while their credential exists so a failed
        // final client disk write can safely retry even after the approval window.
        let expires = (!matches!(enrollment.state, EnrollmentState::Installed { .. }))
            .then_some(enrollment.expires_at);
        self.db
            .put(&key("enrollment", &enrollment.id), enrollment, expires)?;
        self.db.put(
            &key("request", &enrollment.claim_digest.0),
            &enrollment.id,
            expires,
        )
    }

    fn active_credential_count(&self) -> Result<usize> {
        self.db.active_credentials()
    }

    fn credential(&self, id: &[u8; 16]) -> Result<Option<Credential>> {
        self.db.get(&key("credential", id))
    }

    fn list_active_credentials(&self) -> Result<Vec<Credential>> {
        self.db.list_active_credentials()
    }

    fn list_revoked_credentials(
        &self,
        after: Option<&RevokedCredentialPosition>,
        limit: usize,
    ) -> Result<Vec<Credential>> {
        self.db.revoked_credentials(after, limit)
    }

    fn principal_for_installed(&mut self, _credential_id: &[u8; 16]) -> Result<[u8; 16]> {
        Self::owner_principal(self.db)
    }

    fn save_credential(&mut self, credential: &Credential) -> Result<bool> {
        self.db.save_credential(credential)?;
        Ok(true)
    }

    fn cancel_installed_credential(&mut self, id: &[u8; 16], _now: u64) -> Result<()> {
        self.db.remove_credential(*id)?;
        self.db.queue_termination(&text_id(id))
    }
}
