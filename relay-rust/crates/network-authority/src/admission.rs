use crate::{
    Authority, Challenge, Environment, Fault, Rate, RegistrationMode, Store,
    challenges::{CHALLENGE_SECONDS, ProofRedemption},
    store::{key, load, save},
};
use anyhow::Result;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

const SOURCE_BURSTS_FOR_WORK: u64 = 2;
#[derive(Default, Serialize, Deserialize)]
struct Bucket {
    credit: u64,
    updated: u64,
    bursts: u64,
}

impl<R: Store, E: Environment> Authority<R, E> {
    pub(crate) fn creation_admission(
        &mut self,
        source: &str,
        challenge: Option<&str>,
        solution: Option<&str>,
    ) -> Result<Option<String>> {
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
        let email = if mode == RegistrationMode::Automatic {
            None
        } else {
            match self.registration_proof(source_id, mode, challenge, solution)? {
                ProofRedemption::WorkVerified => None,
                ProofRedemption::EmailVerified(address) => Some(address),
                ProofRedemption::Retry(challenge) if mode == RegistrationMode::Email => {
                    return Err(Fault::Verification { challenge }.into());
                }
                // Proof-of-work retries must still match the configured difficulty.
                ProofRedemption::Retry(_) | ProofRedemption::Unavailable => {
                    let challenge = if mode == RegistrationMode::Email {
                        Challenge {
                            id: String::new(),
                            mode,
                            bits: 0,
                            expires_at: now + CHALLENGE_SECONDS,
                        }
                    } else {
                        self.work_challenge(source_id, settings.pow_bits)?
                    };
                    return Err(Fault::Verification { challenge }.into());
                }
            }
        };
        local.credit -= 1;
        global.credit -= 1;
        save(&mut self.store, &source_key, &local)?;
        save(&mut self.store, "creation-global", &global)?;
        Ok(email)
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
