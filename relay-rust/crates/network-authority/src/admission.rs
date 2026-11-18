use crate::{
    Authority, Challenge, Environment, Fault, Rate, RegistrationMode, Store,
    store::{key, load, save},
};
use anyhow::{Result, ensure};
use base64::{Engine, engine::general_purpose::URL_SAFE_NO_PAD};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

const CHALLENGE_SECONDS: u64 = 15 * 60;
const SOURCE_BURSTS_FOR_WORK: u64 = 2;
const MAX_CHALLENGE_ATTEMPTS: u8 = 5;
#[derive(Default, Serialize, Deserialize)]
struct Bucket {
    credit: u64,
    updated: u64,
    bursts: u64,
}
#[derive(Serialize, Deserialize)]
struct Proof {
    challenge: Challenge,
    source: [u8; 32],
    email_digest: Option<[u8; 32]>,
    attempts: u8,
}

impl<R: Store, E: Environment> Authority<R, E> {
    pub(crate) fn creation_admission(
        &mut self,
        source: &str,
        challenge: Option<&str>,
        solution: Option<&str>,
    ) -> Result<()> {
        let settings = self.settings()?;
        let now = self.environment.now();
        let source_id: [u8; 32] = Sha256::digest(source.as_bytes()).into();
        let source_key = key("creation-source", &source_id);
        let mut local = load::<Bucket>(&self.store, &source_key)?.unwrap_or(Bucket {
            credit: settings.source_rate.burst,
            updated: now,
            bursts: 0,
        });
        let mut global = load::<Bucket>(&self.store, "creation-global")?.unwrap_or(Bucket {
            credit: settings.global_rate.burst,
            updated: now,
            bursts: 0,
        });
        recover(&mut local, &settings.source_rate, now);
        recover(&mut global, &settings.global_rate, now);
        if local.credit == 0 || global.credit == 0 {
            local.bursts = local.bursts.saturating_add(1);
            save(&mut self.store, &source_key, &local)?;
            return Err(Fault::RateLimited {
                retry_after: settings
                    .source_rate
                    .refill_seconds
                    .max(settings.global_rate.refill_seconds),
            }
            .into());
        }
        let mode = if settings.registration == RegistrationMode::Automatic
            && local.bursts >= SOURCE_BURSTS_FOR_WORK
        {
            RegistrationMode::ProofOfWork
        } else {
            settings.registration
        };
        if mode != RegistrationMode::Automatic
            && !self.verify_proof(source_id, mode, challenge, solution)?
        {
            let challenge = if mode == RegistrationMode::Email {
                Challenge {
                    id: String::new(),
                    mode,
                    bits: 0,
                    expires_at: now + CHALLENGE_SECONDS,
                }
            } else {
                self.make_proof(source_id, mode, settings.pow_bits, None)?
            };
            return Err(Fault::Verification { challenge }.into());
        }
        local.credit -= 1;
        global.credit -= 1;
        save(&mut self.store, &source_key, &local)?;
        save(&mut self.store, "creation-global", &global)?;
        Ok(())
    }

    fn make_proof(
        &mut self,
        source: [u8; 32],
        mode: RegistrationMode,
        bits: u8,
        email_digest: Option<[u8; 32]>,
    ) -> Result<Challenge> {
        let source_key = key("source-challenge", &source);
        if let Some(id) = load::<String>(&self.store, &source_key)? {
            let previous_key = format!("challenge/{id}");
            if let Some(previous) = load::<Proof>(&self.store, &previous_key)? {
                if email_digest.is_none()
                    && previous.challenge.mode == mode
                    && previous.challenge.bits == bits
                    && previous.challenge.expires_at > self.environment.now()
                    && previous.attempts < MAX_CHALLENGE_ATTEMPTS
                {
                    return Ok(previous.challenge);
                }
                self.store.delete(&previous_key)?;
            }
        }
        let challenge = Challenge {
            id: URL_SAFE_NO_PAD.encode(self.environment.random::<32>()?),
            mode,
            bits,
            expires_at: self.environment.now() + CHALLENGE_SECONDS,
        };
        let proof = Proof {
            challenge: challenge.clone(),
            source,
            email_digest,
            attempts: 0,
        };
        save(
            &mut self.store,
            &format!("challenge/{}", challenge.id),
            &proof,
        )?;
        save(&mut self.store, &source_key, &challenge.id)?;
        Ok(challenge)
    }

    /// The host sends the secret through its configured email provider, never in its HTTP response.
    /// # Errors
    /// Refuses exhausted source limits or unavailable entropy/storage.
    pub fn email_challenge(&mut self, source: &str) -> Result<(Challenge, String)> {
        let source_id: [u8; 32] = Sha256::digest(source.as_bytes()).into();
        let rate_key = key("email-source", &source_id);
        let rate = Rate {
            burst: 3,
            refill_seconds: 60,
        };
        let now = self.environment.now();
        let mut bucket = load::<Bucket>(&self.store, &rate_key)?.unwrap_or(Bucket {
            credit: rate.burst,
            updated: now,
            bursts: 0,
        });
        recover(&mut bucket, &rate, now);
        ensure!(
            bucket.credit > 0,
            Fault::RateLimited {
                retry_after: rate.refill_seconds
            }
        );
        bucket.credit -= 1;
        save(&mut self.store, &rate_key, &bucket)?;
        let secret = URL_SAFE_NO_PAD.encode(self.environment.random::<16>()?);
        let challenge = self.make_proof(
            source_id,
            RegistrationMode::Email,
            0,
            Some(Sha256::digest(secret.as_bytes()).into()),
        )?;
        Ok((challenge, secret))
    }

    fn verify_proof(
        &mut self,
        source: [u8; 32],
        mode: RegistrationMode,
        id: Option<&str>,
        solution: Option<&str>,
    ) -> Result<bool> {
        let (Some(id), Some(solution)) = (id, solution) else {
            return Ok(false);
        };
        ensure!(id.len() <= 64 && solution.len() <= 128, Fault::Invalid);
        let proof_key = format!("challenge/{id}");
        let Some(mut proof) = load::<Proof>(&self.store, &proof_key)? else {
            return Ok(false);
        };
        if proof.source != source
            || proof.challenge.mode != mode
            || proof.challenge.expires_at <= self.environment.now()
            || proof.attempts >= MAX_CHALLENGE_ATTEMPTS
            || mode == RegistrationMode::Email && proof.email_digest.is_none()
        {
            return Ok(false);
        }
        proof.attempts += 1;
        let valid = if let Some(digest) = proof.email_digest {
            digest == <[u8; 32]>::from(Sha256::digest(solution.as_bytes()))
        } else {
            let digest = Sha256::new()
                .chain_update(id.as_bytes())
                .chain_update(solution.as_bytes())
                .finalize();
            let zeroes = digest.iter().take_while(|byte| **byte == 0).count() * 8;
            let zeroes = zeroes
                + digest
                    .get(zeroes / 8)
                    .map_or(0, |byte| byte.leading_zeros() as usize);
            zeroes >= usize::from(proof.challenge.bits)
        };
        if valid {
            self.store.delete(&proof_key)?;
        } else {
            save(&mut self.store, &proof_key, &proof)?;
        }
        Ok(valid)
    }
}
fn recover(bucket: &mut Bucket, rate: &Rate, now: u64) {
    let elapsed = now.saturating_sub(bucket.updated);
    let refill = elapsed / rate.refill_seconds;
    if refill > 0 {
        bucket.credit = bucket.credit.saturating_add(refill).min(rate.burst);
        bucket.updated = now - elapsed % rate.refill_seconds;
        if bucket.credit == rate.burst {
            bucket.bursts = 0;
        }
    }
}
