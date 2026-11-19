use crate::{
    Authority, Challenge, Environment, Fault, RegistrationMode, Store,
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
const WORK_PROOF_PREFIX: &str = "registration-proof";
pub const REGISTRATION_EMAIL_PROOF_PREFIX: &str = "registration-email-proof";

pub enum EmailOwner {
    Registration(RegistrationAttempt),
    Account(AccountId),
}

impl EmailOwner {
    fn key(&self) -> String {
        match self {
            Self::Registration(attempt) => key(REGISTRATION_EMAIL_PROOF_PREFIX, attempt.digest()),
            Self::Account(id) => key("account-email-proof", id),
        }
    }
}

#[derive(Clone, Copy)]
pub enum RegistrationProofOwner {
    Work([u8; 32]),
    Email(RegistrationAttempt),
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
    const fn mode(&self) -> RegistrationMode {
        match self {
            Self::Work { .. } => RegistrationMode::ProofOfWork,
            Self::Email { .. } => RegistrationMode::Email,
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
            mode: self.method.mode(),
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
    pub fn accepted(self, store: &mut impl Store) -> Result<Challenge> {
        let key = self.owner.key();
        save(store, &key, &self.proof)?;
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

    pub(crate) fn work_challenge(&mut self, source: [u8; 32], bits: u8) -> Result<Challenge> {
        let storage_key = key(WORK_PROOF_PREFIX, &source);
        if let Some(proof) = load::<Proof>(&self.store, &storage_key)?
            && matches!(proof.method, ProofMethod::Work { bits: expected } if expected == bits)
            && proof.live(self.environment.now())
        {
            return Ok(proof.into_challenge());
        }
        let proof = self.new_proof(ProofMethod::Work { bits })?;
        save(&mut self.store, &storage_key, &proof)?;
        Ok(proof.into_challenge())
    }

    pub(crate) fn issue_email_code(
        &mut self,
        source: &str,
        owner: EmailOwner,
        address: &str,
    ) -> Result<EmailDelivery> {
        self.throttle(source, "email-source", EMAIL_BURST, EMAIL_REFILL_SECONDS)?;
        if let EmailOwner::Account(_) = owner {
            self.throttle(
                &owner.key(),
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
        owner: RegistrationProofOwner,
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
        let (storage_key, mode) = match owner {
            RegistrationProofOwner::Work(source) => (
                key(WORK_PROOF_PREFIX, &source),
                RegistrationMode::ProofOfWork,
            ),
            RegistrationProofOwner::Email(attempt) => (
                EmailOwner::Registration(attempt).key(),
                RegistrationMode::Email,
            ),
        };
        self.redeem_proof(&storage_key, solution, |proof| {
            proof.id == id && proof.method.mode() == mode
        })
    }

    pub(crate) fn verify_mailbox(
        &mut self,
        account: AccountId,
        address: &str,
        id: &str,
        code: &str,
    ) -> Result<()> {
        let storage_key = EmailOwner::Account(account).key();
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
            tracing::info!(event = "verification_code_rejected", challenge = %proof.id, attempts = proof.attempts);
            save(&mut self.store, storage_key, &proof)?;
            // The rejected attempt counts before the proof can be offered for another try.
            return Ok(if proof.live(now) {
                ProofRedemption::Retry(proof.into_challenge())
            } else {
                ProofRedemption::Unavailable
            });
        }
        self.store.delete(storage_key)?;
        Ok(match proof.method {
            ProofMethod::Work { .. } => ProofRedemption::WorkVerified,
            ProofMethod::Email { address, .. } => ProofRedemption::EmailVerified(address),
        })
    }
}
