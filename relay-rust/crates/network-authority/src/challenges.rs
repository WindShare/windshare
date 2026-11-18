use crate::{
    Authority, Challenge, Environment, Fault, RegistrationMode, Store,
    registration::digest,
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

pub enum Owner {
    Registration([u8; 32]),
    Account(AccountId),
}

impl Owner {
    fn key(&self) -> String {
        match self {
            Self::Registration(source) => key("registration-proof", source),
            Self::Account(id) => key("account-email-proof", id),
        }
    }
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

    fn challenge(&self) -> Challenge {
        Challenge {
            id: self.id.clone(),
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

pub enum Verified {
    Work,
    Email(String),
}

pub struct EmailDelivery {
    owner: Owner,
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
        tracing::info!(event="email_verification_delivered", owner=key, challenge=%self.proof.id);
        Ok(self.proof.challenge())
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
        let owner = Owner::Registration(source);
        if let Some(proof) = load::<Proof>(&self.store, &owner.key())?
            && matches!(proof.method, ProofMethod::Work { bits: expected } if expected == bits)
            && proof.live(self.environment.now())
        {
            return Ok(proof.challenge());
        }
        let proof = self.new_proof(ProofMethod::Work { bits })?;
        save(&mut self.store, &owner.key(), &proof)?;
        Ok(proof.challenge())
    }

    pub(crate) fn issue_email_code(
        &mut self,
        source: &str,
        owner: Owner,
        address: &str,
    ) -> Result<EmailDelivery> {
        self.throttle(source, "email-source", EMAIL_BURST, EMAIL_REFILL_SECONDS)?;
        if let Owner::Account(_) = owner {
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
        tracing::info!(event = "email_verification_requested", challenge = %proof.id, owner = owner.key());
        Ok(EmailDelivery { owner, proof, code })
    }

    pub(crate) fn registration_proof(
        &mut self,
        source: [u8; 32],
        mode: RegistrationMode,
        id: Option<&str>,
        solution: Option<&str>,
    ) -> Result<Option<Verified>> {
        let (Some(id), Some(solution)) = (id, solution) else {
            return Ok(None);
        };
        ensure!(
            id.len() <= MAX_CHALLENGE_ID_BYTES && solution.len() <= MAX_SOLUTION_BYTES,
            Fault::Invalid
        );
        self.redeem_proof(&Owner::Registration(source), solution, |proof| {
            proof.method.mode() == mode && proof.id == id
        })
    }

    pub(crate) fn verify_mailbox(
        &mut self,
        account: AccountId,
        address: &str,
        id: &str,
        code: &str,
    ) -> Result<()> {
        let result = self.redeem_proof(&Owner::Account(account), code, |proof| {
            proof.id == id && matches!(&proof.method, ProofMethod::Email { address: target, .. } if target == address)
        })?;
        ensure!(
            matches!(result, Some(Verified::Email(_))),
            Fault::EmailCodeRejected
        );
        Ok(())
    }

    fn redeem_proof(
        &mut self,
        owner: &Owner,
        solution: &str,
        matches: impl FnOnce(&Proof) -> bool,
    ) -> Result<Option<Verified>> {
        let storage_key = owner.key();
        let Some(mut proof) = load::<Proof>(&self.store, &storage_key)? else {
            return Ok(None);
        };
        if !matches(&proof) || !proof.live(self.environment.now()) {
            return Ok(None);
        }
        if solution.len() > MAX_SOLUTION_BYTES || !proof.accepts(solution) {
            proof.attempts += 1;
            tracing::info!(event = "verification_code_rejected", challenge = %proof.id, attempts = proof.attempts);
            save(&mut self.store, &storage_key, &proof)?;
            return Ok(None);
        }
        self.store.delete(&storage_key)?;
        Ok(Some(match proof.method {
            ProofMethod::Work { .. } => Verified::Work,
            ProofMethod::Email { address, .. } => Verified::Email(address),
        }))
    }
}
