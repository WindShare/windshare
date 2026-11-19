use crate::{
    Authority, Challenge, Environment, Fault, RegistrationMode, Settings, Store,
    registration::{RegistrationAttempt, digest},
    store::{key, load, save},
};
use anyhow::{Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use windshare_relay_access::AccountId;

pub const CHALLENGE_SECONDS: u64 = 15 * 60;
const MAX_ATTEMPTS: u8 = 5;
const MAX_SOLUTION_BYTES: usize = 128;
const MAX_CHALLENGE_ID_BYTES: usize = 64;
const EMAIL_BURST: u64 = 3;
const EMAIL_REFILL_SECONDS: u64 = 60;
#[derive(Clone, Copy, PartialEq, Eq)]
pub enum ProofKind {
    Work,
    Email,
}

impl ProofKind {
    pub(crate) const fn registration_prefix(self) -> &'static str {
        match self {
            Self::Work => "registration-proof",
            Self::Email => "registration-email-proof",
        }
    }

    fn registration_key(self, attempt: RegistrationAttempt) -> String {
        key(self.registration_prefix(), attempt.digest())
    }

    const fn mode(self) -> RegistrationMode {
        match self {
            Self::Work => RegistrationMode::ProofOfWork,
            Self::Email => RegistrationMode::Email,
        }
    }
}

pub enum EmailOwner {
    Registration(RegistrationAttempt),
    // Released clients (v0.1.3) send their setup secret only with the code, so the
    // challenge id returned to the requester binds the proof instead.
    ReleasedRegistration,
    Account(AccountId),
}

impl EmailOwner {
    fn key(&self, challenge: &str) -> String {
        match self {
            Self::Registration(attempt) => ProofKind::Email.registration_key(*attempt),
            Self::ReleasedRegistration => released_registration_email_key(challenge),
            Self::Account(id) => account_email_key(id),
        }
    }
}

fn account_email_key(id: &AccountId) -> String {
    key("account-email-proof", id)
}

// The domain prefix keeps a challenge digest distinct from a setup secret digest.
fn released_registration_email_key(challenge: &str) -> String {
    key(
        ProofKind::Email.registration_prefix(),
        &digest(&format!("challenge/{challenge}")),
    )
}

#[derive(Serialize, Deserialize)]
enum ProofMethod {
    Work {
        bits: u8,
    },
    Email {
        address: String,
        code_digest: [u8; 32],
    },
}

impl ProofMethod {
    const fn kind(&self) -> ProofKind {
        match self {
            Self::Work { .. } => ProofKind::Work,
            Self::Email { .. } => ProofKind::Email,
        }
    }
}

#[derive(Serialize, Deserialize)]
struct Proof {
    id: String,
    expires_at: u64,
    attempts: u8,
    method: ProofMethod,
}

impl Proof {
    const fn live(&self, now: u64) -> bool {
        self.expires_at > now && self.attempts < MAX_ATTEMPTS
    }

    fn into_challenge(self) -> Challenge {
        Challenge {
            id: self.id,
            mode: self.method.kind().mode(),
            bits: match self.method {
                ProofMethod::Work { bits } => bits,
                ProofMethod::Email { .. } => 0,
            },
            expires_at: self.expires_at,
        }
    }

    fn accepts(&self, solution: &str) -> bool {
        match &self.method {
            ProofMethod::Email { code_digest, .. } => digest(solution) == *code_digest,
            ProofMethod::Work { bits } => {
                let hash = Sha256::new()
                    .chain_update(self.id.as_bytes())
                    .chain_update(solution.as_bytes())
                    .finalize();
                let bytes = hash.iter().take_while(|byte| **byte == 0).count();
                let zeroes = bytes * 8
                    + hash
                        .get(bytes)
                        .map_or(0, |byte| byte.leading_zeros() as usize);
                zeroes >= usize::from(*bits)
            }
        }
    }
}

pub enum ProofRedemption {
    WorkVerified,
    EmailVerified(String),
    Retry(Challenge),
    Unavailable,
}

pub struct EmailDelivery {
    owner: EmailOwner,
    proof: Proof,
    code: String,
}

impl EmailDelivery {
    pub fn code(&self) -> &str {
        &self.code
    }

    pub fn challenge_id(&self) -> &str {
        &self.proof.id
    }

    /// # Errors
    /// Only a successful provider delivery should replace the previously usable code.
    pub fn accepted(self, store: &mut impl Store, now: u64) -> Result<Challenge> {
        if !matches!(self.owner, EmailOwner::Account(_)) {
            store.prune_registration_proofs(ProofKind::Email, now)?;
        }
        save(store, &self.owner.key(&self.proof.id), &self.proof)?;
        tracing::info!(event = "email_verification_delivered", challenge = %self.proof.id);
        Ok(self.proof.into_challenge())
    }
}

impl<R: Store, E: Environment> Authority<R, E> {
    fn new_proof(&mut self, method: ProofMethod) -> Result<Proof> {
        Ok(Proof {
            id: URL_SAFE_NO_PAD.encode(self.environment.random::<32>()?),
            expires_at: self.environment.now() + CHALLENGE_SECONDS,
            attempts: 0,
            method,
        })
    }

    pub(crate) fn work_challenge(
        &mut self,
        attempt: RegistrationAttempt,
        source_id: &[u8; 32],
        settings: &Settings,
    ) -> Result<Challenge> {
        let kind = ProofKind::Work;
        let storage_key = kind.registration_key(attempt);
        let now = self.environment.now();
        let bits = settings.pow_bits;
        if let Some(proof) = load::<Proof>(&self.store, &storage_key)?
            && matches!(proof.method, ProofMethod::Work { bits: expected } if expected == bits)
            && proof.live(now)
        {
            return Ok(proof.into_challenge());
        }
        // Retrieving the same challenge spends capacity only when it needs replacement.
        self.charge_work_issuance(source_id, settings, now)?;
        let proof = self.new_proof(ProofMethod::Work { bits })?;
        self.store.prune_registration_proofs(kind, now)?;
        save(&mut self.store, &storage_key, &proof)?;
        tracing::debug!(
            event = "registration_work_challenge_issued",
            challenge = %proof.id,
            bits,
            expires_at = proof.expires_at
        );
        Ok(proof.into_challenge())
    }

    pub(crate) fn issue_email_code(
        &mut self,
        source: &str,
        owner: EmailOwner,
        address: &str,
    ) -> Result<EmailDelivery> {
        self.throttle(source, "email-source", EMAIL_BURST, EMAIL_REFILL_SECONDS)?;
        if let EmailOwner::Account(id) = &owner {
            self.throttle(
                &account_email_key(id),
                "email-account",
                EMAIL_BURST,
                EMAIL_REFILL_SECONDS,
            )?;
        }
        let code = URL_SAFE_NO_PAD.encode(self.environment.random::<16>()?);
        let method = ProofMethod::Email {
            address: address.into(),
            code_digest: digest(&code),
        };
        let proof = self.new_proof(method)?;
        tracing::info!(event = "email_verification_requested", challenge = %proof.id);
        Ok(EmailDelivery { owner, proof, code })
    }

    pub(crate) fn registration_proof(
        &mut self,
        attempt: RegistrationAttempt,
        kind: ProofKind,
        id: Option<&str>,
        solution: Option<&str>,
    ) -> Result<ProofRedemption> {
        let (Some(id), Some(solution)) = (id, solution) else {
            return Ok(ProofRedemption::Unavailable);
        };
        ensure!(
            id.len() <= MAX_CHALLENGE_ID_BYTES && solution.len() <= MAX_SOLUTION_BYTES,
            Fault::Invalid
        );
        let redemption = self.redeem_proof(&kind.registration_key(attempt), solution, |proof| {
            proof.id == id && proof.method.kind() == kind
        })?;
        if kind != ProofKind::Email || !matches!(redemption, ProofRedemption::Unavailable) {
            return Ok(redemption);
        }
        self.redeem_proof(&released_registration_email_key(id), solution, |proof| {
            proof.id == id && proof.method.kind() == kind
        })
    }

    pub(crate) fn verify_mailbox(
        &mut self,
        account: AccountId,
        address: &str,
        id: &str,
        code: &str,
    ) -> Result<()> {
        let storage_key = account_email_key(&account);
        let result = self.redeem_proof(&storage_key, code, |proof| {
            proof.id == id && matches!(&proof.method, ProofMethod::Email { address: target, .. } if target == address)
        })?;
        ensure!(
            matches!(result, ProofRedemption::EmailVerified(_)),
            Fault::EmailCodeRejected
        );
        Ok(())
    }

    fn redeem_proof(
        &mut self,
        storage_key: &str,
        solution: &str,
        matches: impl FnOnce(&Proof) -> bool,
    ) -> Result<ProofRedemption> {
        let Some(mut proof) = load::<Proof>(&self.store, storage_key)? else {
            return Ok(ProofRedemption::Unavailable);
        };
        let now = self.environment.now();
        if !matches(&proof) || !proof.live(now) {
            return Ok(ProofRedemption::Unavailable);
        }
        if solution.len() > MAX_SOLUTION_BYTES || !proof.accepts(solution) {
            proof.attempts += 1;
            tracing::info!(
                event = "verification_proof_rejected",
                challenge = %proof.id,
                mode = ?proof.method.kind().mode(),
                attempts = proof.attempts
            );
            save(&mut self.store, storage_key, &proof)?;
            // The rejected attempt counts before the proof can be offered for another try.
            return Ok(if proof.live(now) {
                ProofRedemption::Retry(proof.into_challenge())
            } else {
                ProofRedemption::Unavailable
            });
        }
        self.store.delete(storage_key)?;
        tracing::debug!(
            event = "verification_proof_redeemed",
            challenge = %proof.id,
            mode = ?proof.method.kind().mode()
        );
        Ok(match proof.method {
            ProofMethod::Work { .. } => ProofRedemption::WorkVerified,
            ProofMethod::Email { address, .. } => ProofRedemption::EmailVerified(address),
        })
    }
}
