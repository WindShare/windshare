//! Shared synchronous orchestration for device enrollment and credential management.
//!
//! Host-specific concerns—admin authentication, session persistence, rate-limit policy,
//! account-authority bindings, and transport framing—stay in each host adapter.
use crate::{
    MAX_ACTIVE_CREDENTIALS, MAX_RECORDS, claimed_key, credential_view,
    credentials::{CREDENTIAL_HISTORY_PAGE_SIZE, RevokedCredentialPosition, credential_history},
    http::{ConsoleError, optional_bool, required_id, required_str},
    new_access_key, pending_enrollment_view, service_configuration, text_id, user_code,
};
use serde_json::{Value, json};
use windshare_relay_access::{
    Credential, CredentialState, Digest, Enrollment, EnrollmentState, Poll, Refusal, validate_name,
};

const ENROLLMENT_CAPACITY_RETRY_MS: u64 = 60_000;

#[derive(Debug)]
pub enum AccessError<E> {
    Rejected(ConsoleError),
    Store(E),
}

impl<E> From<ConsoleError> for AccessError<E> {
    fn from(error: ConsoleError) -> Self {
        Self::Rejected(error)
    }
}

impl<E> From<Refusal> for AccessError<E> {
    fn from(error: Refusal) -> Self {
        Self::Rejected(ConsoleError::from_refusal(error))
    }
}

/// Persistence and entropy port implemented by each relay host.
pub trait AccessStore {
    type Error;

    /// Fills `dest` with cryptographically secure random bytes.
    ///
    /// # Errors
    /// Propagates host entropy failures as store errors.
    fn fill_random(&mut self, dest: &mut [u8]) -> Result<(), Self::Error>;

    /// Enforces host-specific enrollment rate limits before creating a new request.
    ///
    /// # Errors
    /// Returns [`AccessError::Rejected`] when throttled, or [`AccessError::Store`] on I/O error.
    fn check_enrollment_rate_limit(&mut self, now: u64) -> Result<(), AccessError<Self::Error>>;

    /// Counts non-installed enrollment records still bounded by an expiry window.
    ///
    /// # Errors
    /// Propagates host storage errors.
    fn pending_enrollment_count(&self) -> Result<usize, Self::Error>;

    /// Looks up an existing enrollment by claimant digest for idempotent retries.
    ///
    /// # Errors
    /// Propagates host storage errors.
    fn enrollment_by_claimant(&self, claimant: Digest) -> Result<Option<Enrollment>, Self::Error>;

    /// Loads an enrollment by identifier.
    ///
    /// # Errors
    /// Propagates host storage errors.
    fn enrollment(&self, id: &[u8; 16]) -> Result<Option<Enrollment>, Self::Error>;

    /// Lists enrollment records that may still require administrator approval.
    ///
    /// # Errors
    /// Propagates host storage errors.
    fn list_enrollments(&self) -> Result<Vec<Enrollment>, Self::Error>;

    /// Persists an enrollment, retaining installed receipts without an expiry deadline.
    ///
    /// # Errors
    /// Propagates host storage errors.
    fn save_enrollment(&mut self, enrollment: &Enrollment) -> Result<(), Self::Error>;

    /// Counts currently active credentials for capacity enforcement.
    ///
    /// # Errors
    /// Propagates host storage errors.
    fn active_credential_count(&self) -> Result<usize, Self::Error>;

    /// Loads a credential by identifier.
    ///
    /// # Errors
    /// Propagates host storage errors.
    fn credential(&self, id: &[u8; 16]) -> Result<Option<Credential>, Self::Error>;

    /// Lists active credentials for the console inventory.
    ///
    /// # Errors
    /// Propagates host storage errors.
    fn list_active_credentials(&self) -> Result<Vec<Credential>, Self::Error>;

    /// Lists at most `limit` revoked credentials strictly after `after` in reverse revocation order.
    ///
    /// # Errors
    /// Propagates host storage errors.
    fn list_revoked_credentials(
        &self,
        after: Option<&RevokedCredentialPosition>,
        limit: usize,
    ) -> Result<Vec<Credential>, Self::Error>;

    /// Resolves the principal that owns a newly installed enrollment credential.
    ///
    /// # Errors
    /// Propagates host storage errors.
    fn principal_for_installed(
        &mut self,
        credential_id: &[u8; 16],
    ) -> Result<[u8; 16], Self::Error>;

    /// Persists a created or revoked credential. Returns `true` when durable state changed.
    ///
    /// # Errors
    /// Propagates host storage errors.
    fn save_credential(&mut self, credential: &Credential) -> Result<bool, Self::Error>;

    /// Reverses an installed credential when a claimant cancels after delivery.
    ///
    /// # Errors
    /// Propagates host storage errors.
    fn cancel_installed_credential(&mut self, id: &[u8; 16], now: u64) -> Result<(), Self::Error>;
}

pub struct EnrollOutcome {
    pub enrollment: Enrollment,
    pub created: bool,
}

pub struct ClaimOutcome {
    pub enrollment_id: [u8; 16],
    pub credential_id: [u8; 16],
    pub body: Value,
}

pub struct InstallOutcome {
    pub enrollment_id: [u8; 16],
    pub credential_id: [u8; 16],
    pub newly_installed: bool,
}

pub struct CancelOutcome {
    pub enrollment_id: [u8; 16],
    pub found: bool,
    pub cancelled_credential: Option<[u8; 16]>,
}

pub struct ApproveOutcome {
    pub enrollment_id: [u8; 16],
}

pub struct CreatedCredential {
    pub credential: Credential,
    pub body: Value,
}

pub struct RevokedCredential {
    pub credential: Credential,
    pub changed: bool,
    pub terminate_shares: bool,
}

pub struct RevokedHistory {
    pub continued: bool,
    pub has_more: bool,
    pub body: Value,
}

fn random_array<const N: usize, S: AccessStore>(store: &mut S) -> Result<[u8; N], S::Error> {
    let mut bytes = [0_u8; N];
    store.fill_random(&mut bytes)?;
    Ok(bytes)
}

fn require_credential_capacity<S: AccessStore>(store: &S) -> Result<(), AccessError<S::Error>> {
    if store
        .active_credential_count()
        .map_err(AccessError::Store)?
        >= MAX_ACTIVE_CREDENTIALS
    {
        return Err(ConsoleError::too_many_requests("credential_capacity", 0).into());
    }
    Ok(())
}

/// Starts or idempotently recovers a device enrollment request.
///
/// # Errors
/// Returns [`AccessError::Rejected`] for malformed input, invalid names, rate limits,
/// or enrollment capacity saturation, and [`AccessError::Store`] on persistence failure.
pub fn enroll<S: AccessStore>(
    store: &mut S,
    body: &Value,
    now: u64,
) -> Result<EnrollOutcome, AccessError<S::Error>> {
    let device_name = required_str(body, "device_name")?;
    let claim_digest = Digest(required_id(body, "claim_digest")?);
    if let Some(existing) = store
        .enrollment_by_claimant(claim_digest)
        .map_err(AccessError::Store)?
    {
        return Ok(EnrollOutcome {
            enrollment: existing,
            created: false,
        });
    }
    store.check_enrollment_rate_limit(now)?;
    if store
        .pending_enrollment_count()
        .map_err(AccessError::Store)?
        >= MAX_RECORDS
    {
        return Err(
            ConsoleError::too_many_requests("slow_down", ENROLLMENT_CAPACITY_RETRY_MS).into(),
        );
    }
    let code = user_code(random_array(store).map_err(AccessError::Store)?);
    let enrollment_id = random_array(store).map_err(AccessError::Store)?;
    let enrollment = Enrollment::new(
        enrollment_id,
        code,
        device_name.to_owned(),
        claim_digest,
        now,
    )?;
    store
        .save_enrollment(&enrollment)
        .map_err(AccessError::Store)?;
    Ok(EnrollOutcome {
        enrollment,
        created: true,
    })
}

/// Polls an enrollment and derives the recoverable access key once approved.
///
/// Activation waits for [`installed`] so abandoned claims never materialize live keys.
///
/// # Errors
/// Returns [`AccessError::Rejected`] for invalid input, expired/pending/denied states,
/// or rate-limited polling, and [`AccessError::Store`] on persistence failure.
pub fn claim<S: AccessStore>(
    store: &mut S,
    body: &Value,
    now: u64,
) -> Result<ClaimOutcome, AccessError<S::Error>> {
    let enrollment_id = required_id(body, "enrollment_id")?;
    let secret: [u8; 32] = required_id(body, "claim_secret")?;
    let Some(mut enrollment) = store
        .enrollment(&enrollment_id)
        .map_err(AccessError::Store)?
    else {
        return Err(Refusal::Expired.into());
    };
    let result = enrollment.poll(Digest::of(&secret), now)?;
    store
        .save_enrollment(&enrollment)
        .map_err(AccessError::Store)?;
    match result {
        Poll::Pending => Err(Refusal::Pending.into()),
        Poll::Approved { credential } | Poll::Installed { credential } => {
            let saved = store.credential(&credential).map_err(AccessError::Store)?;
            if saved
                .as_ref()
                .is_some_and(|saved| saved.state != CredentialState::Active)
                || (saved.is_none() && matches!(result, Poll::Installed { .. }))
            {
                return Err(Refusal::AccessDenied.into());
            }
            let access_key = claimed_key(&secret);
            Ok(ClaimOutcome {
                enrollment_id: enrollment.id,
                credential_id: credential,
                body: json!({
                    "access_key": access_key,
                    "credential_id": text_id(&credential),
                    "expires_at": enrollment.expires_at,
                }),
            })
        }
    }
}

/// Confirms client installation and activates the derived credential idempotently.
///
/// # Errors
/// Returns [`AccessError::Rejected`] when expired, not yet claimed, inactive, or at capacity,
/// and [`AccessError::Store`] on persistence failure.
pub fn installed<S: AccessStore>(
    store: &mut S,
    body: &Value,
    now: u64,
) -> Result<InstallOutcome, AccessError<S::Error>> {
    let enrollment_id = required_id(body, "enrollment_id")?;
    let secret: [u8; 32] = required_id(body, "claim_secret")?;
    let Some(mut enrollment) = store
        .enrollment(&enrollment_id)
        .map_err(AccessError::Store)?
    else {
        return Err(Refusal::Expired.into());
    };
    let already_installed = matches!(enrollment.state, EnrollmentState::Installed { .. });
    enrollment.installed(Digest::of(&secret), now)?;
    let EnrollmentState::Installed { credential } = enrollment.state else {
        return Err(Refusal::Pending.into());
    };
    if already_installed {
        let active = store
            .credential(&credential)
            .map_err(AccessError::Store)?
            .is_some_and(|saved| saved.state == CredentialState::Active);
        if !active {
            return Err(Refusal::AccessDenied.into());
        }
        return Ok(InstallOutcome {
            enrollment_id: enrollment.id,
            credential_id: credential,
            newly_installed: false,
        });
    }
    require_credential_capacity(store)?;
    let access_key = claimed_key(&secret);
    let principal = store
        .principal_for_installed(&credential)
        .map_err(AccessError::Store)?;
    store
        .save_credential(&Credential {
            id: credential,
            principal,
            name: enrollment.device_name.clone(),
            digest: Digest::of(access_key.as_bytes()),
            created_at: now,
            last_used_at: None,
            state: CredentialState::Active,
        })
        .map_err(AccessError::Store)?;
    store
        .save_enrollment(&enrollment)
        .map_err(AccessError::Store)?;
    Ok(InstallOutcome {
        enrollment_id: enrollment.id,
        credential_id: credential,
        newly_installed: true,
    })
}

/// Cancels an enrollment and rolls back any installed credential for that transaction.
///
/// # Errors
/// Returns [`AccessError::Rejected`] for malformed input or a mismatched claim secret,
/// and [`AccessError::Store`] on persistence failure.
pub fn cancel<S: AccessStore>(
    store: &mut S,
    body: &Value,
    now: u64,
) -> Result<CancelOutcome, AccessError<S::Error>> {
    let enrollment_id = required_id(body, "enrollment_id")?;
    let secret: [u8; 32] = required_id(body, "claim_secret")?;
    let Some(mut enrollment) = store
        .enrollment(&enrollment_id)
        .map_err(AccessError::Store)?
    else {
        return Ok(CancelOutcome {
            enrollment_id,
            found: false,
            cancelled_credential: None,
        });
    };
    let credential = enrollment.cancel(Digest::of(&secret))?;
    if let Some(credential_id) = credential {
        store
            .cancel_installed_credential(&credential_id, now)
            .map_err(AccessError::Store)?;
    }
    store
        .save_enrollment(&enrollment)
        .map_err(AccessError::Store)?;
    Ok(CancelOutcome {
        enrollment_id: enrollment.id,
        found: true,
        cancelled_credential: credential,
    })
}

/// Lists pending enrollment requests awaiting administrator approval.
///
/// # Errors
/// Propagates host storage errors as [`AccessError::Store`].
pub fn list_pending_enrollments<S: AccessStore>(store: &S) -> Result<Value, AccessError<S::Error>> {
    let enrollments: Vec<_> = store
        .list_enrollments()
        .map_err(AccessError::Store)?
        .iter()
        .filter(|e| e.state == EnrollmentState::Pending)
        .map(pending_enrollment_view)
        .collect();
    Ok(json!({"enrollments": enrollments}))
}

/// Approves a pending enrollment after confirming the device-displayed user code.
///
/// # Errors
/// Returns [`AccessError::Rejected`] for invalid input, wrong user code, expiry, or
/// credential capacity limits, and [`AccessError::Store`] on persistence failure.
pub fn approve<S: AccessStore>(
    store: &mut S,
    body: &Value,
    now: u64,
) -> Result<ApproveOutcome, AccessError<S::Error>> {
    let enrollment_id = required_id(body, "enrollment_id")?;
    let supplied_code = required_str(body, "user_code")?;
    let Some(mut enrollment) = store
        .enrollment(&enrollment_id)
        .map_err(AccessError::Store)?
    else {
        return Err(Refusal::Expired.into());
    };
    if supplied_code != enrollment.user_code {
        return Err(Refusal::AccessDenied.into());
    }
    require_credential_capacity(store)?;
    let credential_id = random_array(store).map_err(AccessError::Store)?;
    enrollment.approve(credential_id, now)?;
    store
        .save_enrollment(&enrollment)
        .map_err(AccessError::Store)?;
    Ok(ApproveOutcome { enrollment_id })
}

/// Lists currently active credentials for the console inventory.
///
/// # Errors
/// Propagates host storage errors as [`AccessError::Store`].
pub fn list_active_credentials<S: AccessStore>(store: &S) -> Result<Value, AccessError<S::Error>> {
    let credentials: Vec<_> = store
        .list_active_credentials()
        .map_err(AccessError::Store)?
        .iter()
        .map(credential_view)
        .collect();
    Ok(json!({"credentials": credentials}))
}

/// Lists one page of revoked credentials after an optional keyset cursor.
///
/// # Errors
/// Returns `400 invalid_cursor` when `after_cursor` is malformed, or [`AccessError::Store`].
pub fn list_revoked_credentials<S: AccessStore>(
    store: &S,
    after_cursor: Option<&str>,
) -> Result<RevokedHistory, AccessError<S::Error>> {
    let after = after_cursor
        .map(|cursor| {
            RevokedCredentialPosition::parse(cursor)
                .ok_or_else(|| ConsoleError::bad_request("invalid_cursor"))
        })
        .transpose()?;
    let page = credential_history(
        store
            .list_revoked_credentials(after.as_ref(), CREDENTIAL_HISTORY_PAGE_SIZE + 1)
            .map_err(AccessError::Store)?,
    );
    Ok(RevokedHistory {
        continued: after.is_some(),
        has_more: !page["next_cursor"].is_null(),
        body: page,
    })
}

/// Issues a named manual service key with its own isolated principal.
///
/// # Errors
/// Returns [`AccessError::Rejected`] for missing/invalid names or credential capacity,
/// and [`AccessError::Store`] on persistence failure.
pub fn create_service_key<S: AccessStore>(
    store: &mut S,
    body: &Value,
    relay_url: &str,
    now: u64,
) -> Result<CreatedCredential, AccessError<S::Error>> {
    let name = required_str(body, "name")?;
    validate_name(name)?;
    require_credential_capacity(store)?;
    let access_key = new_access_key(random_array(store).map_err(AccessError::Store)?);
    let credential = Credential {
        id: random_array(store).map_err(AccessError::Store)?,
        principal: random_array(store).map_err(AccessError::Store)?,
        name: name.to_owned(),
        digest: Digest::of(access_key.as_bytes()),
        created_at: now,
        last_used_at: None,
        state: CredentialState::Active,
    };
    store
        .save_credential(&credential)
        .map_err(AccessError::Store)?;
    let configuration = service_configuration(relay_url, &access_key);
    let body = json!({
        "credential": credential_view(&credential),
        "access_key": access_key,
        "configuration": configuration,
    });
    Ok(CreatedCredential { credential, body })
}

/// Revokes an existing credential idempotently.
///
/// # Errors
/// Returns `400 invalid_request` for malformed fields, `404 not_found` when unknown,
/// or [`AccessError::Store`] on persistence failure.
pub fn revoke_credential<S: AccessStore>(
    store: &mut S,
    body: &Value,
    now: u64,
) -> Result<RevokedCredential, AccessError<S::Error>> {
    let credential_id = required_id(body, "credential_id")?;
    let terminate_shares = optional_bool(body, "terminate_shares")?;
    let Some(mut credential) = store
        .credential(&credential_id)
        .map_err(AccessError::Store)?
    else {
        return Err(ConsoleError::not_found().into());
    };
    credential.revoke(now);
    let changed = store
        .save_credential(&credential)
        .map_err(AccessError::Store)?;
    Ok(RevokedCredential {
        credential,
        changed,
        terminate_shares,
    })
}

#[cfg(test)]
#[path = "access_tests.rs"]
mod tests;
