use crate::{
    Authority, Challenge, Environment, Fault, Rate, Registration, RegistrationMode, Settings,
    Store,
    challenges::{CHALLENGE_SECONDS, ProofKind, ProofRedemption},
    registration::RegistrationAttempt,
    store::{key, load, save},
};
use anyhow::Result;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

const SOURCE_BURSTS_FOR_WORK: u64 = 2;
const WORK_ISSUANCE_HEADROOM: u64 = 4;
const GLOBAL_CREATION_KEY: &str = "creation-global";
const GLOBAL_WORK_ISSUANCE_KEY: &str = "work-issuance-global";

#[derive(Serialize, Deserialize)]
struct TokenBucket {
    credit: u64,
    updated: u64,
}

impl TokenBucket {
    const fn new(rate: &Rate, now: u64) -> Self {
        Self {
            credit: rate.burst,
            updated: now,
        }
    }

    fn load(store: &impl Store, key: &str, rate: &Rate, now: u64) -> Result<Self> {
        let mut bucket = load::<Self>(store, key)?.unwrap_or_else(|| Self::new(rate, now));
        bucket.refill(rate, now);
        Ok(bucket)
    }

    fn refill(&mut self, rate: &Rate, now: u64) -> bool {
        let elapsed = now.saturating_sub(self.updated);
        let refill = elapsed / rate.refill_seconds;
        if refill == 0 {
            return false;
        }
        self.credit = self.credit.saturating_add(refill).min(rate.burst);
        self.updated = now - elapsed % rate.refill_seconds;
        true
    }

    const fn retry_after(&self, rate: &Rate, now: u64) -> u64 {
        if self.credit == 0 {
            rate.refill_seconds
                .saturating_sub(now.saturating_sub(self.updated))
        } else {
            0
        }
    }
}

#[derive(Serialize, Deserialize)]
struct SourceCreation {
    #[serde(flatten)]
    bucket: TokenBucket,
    bursts: u64,
}

impl<R: Store, E: Environment> Authority<R, E> {
    pub(crate) fn creation_admission(
        &mut self,
        source: &str,
        attempt: RegistrationAttempt,
        request: &Registration,
        settings: &Settings,
        account_count: u64,
    ) -> Result<Option<String>> {
        let now = self.environment.now();
        let source_id: [u8; 32] = Sha256::digest(source.as_bytes()).into();
        let source_key = key("creation-source", &source_id);
        let mut local =
            load::<SourceCreation>(&self.store, &source_key)?.unwrap_or_else(|| SourceCreation {
                bucket: TokenBucket::new(&settings.source_rate, now),
                bursts: 0,
            });
        let mut global =
            TokenBucket::load(&self.store, GLOBAL_CREATION_KEY, &settings.global_rate, now)?;
        if local.bucket.refill(&settings.source_rate, now)
            && local.bucket.credit == settings.source_rate.burst
        {
            local.bursts = 0;
        }
        if local.bucket.credit == 0 || global.credit == 0 {
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
        let policy_mode = settings.registration.mode(
            account_count,
            self.environment.registration_email_configured(),
        );
        let mode = if policy_mode == RegistrationMode::Automatic
            && local.bursts >= SOURCE_BURSTS_FOR_WORK
        {
            RegistrationMode::ProofOfWork
        } else {
            policy_mode
        };
        let kind = match mode {
            RegistrationMode::Automatic => None,
            RegistrationMode::ProofOfWork => Some(ProofKind::Work),
            RegistrationMode::Email => Some(ProofKind::Email),
        };
        let email = if let Some(kind) = kind {
            match self.registration_proof(
                attempt,
                kind,
                request.challenge.as_deref(),
                request.solution.as_deref(),
            )? {
                ProofRedemption::WorkVerified => None,
                ProofRedemption::EmailVerified(address) => Some(address),
                ProofRedemption::Retry(challenge) if kind == ProofKind::Email => {
                    return Err(Fault::Verification { challenge }.into());
                }
                ProofRedemption::Retry(_) | ProofRedemption::Unavailable => {
                    let challenge = match kind {
                        ProofKind::Email => Challenge {
                            id: String::new(),
                            mode,
                            bits: 0,
                            expires_at: now + CHALLENGE_SECONDS,
                        },
                        ProofKind::Work => self.work_challenge(attempt, &source_id, settings)?,
                    };
                    return Err(Fault::Verification { challenge }.into());
                }
            }
        } else {
            None
        };
        local.bucket.credit -= 1;
        global.credit -= 1;
        save(&mut self.store, &source_key, &local)?;
        save(&mut self.store, GLOBAL_CREATION_KEY, &global)?;
        Ok(email)
    }

    pub(crate) fn charge_work_issuance(
        &mut self,
        source_id: &[u8; 32],
        settings: &Settings,
        now: u64,
    ) -> Result<()> {
        let source_key = key("work-issuance-source", source_id);
        let source_rate = work_issuance_rate(&settings.source_rate);
        let global_rate = work_issuance_rate(&settings.global_rate);
        let mut local = TokenBucket::load(&self.store, &source_key, &source_rate, now)?;
        let mut global =
            TokenBucket::load(&self.store, GLOBAL_WORK_ISSUANCE_KEY, &global_rate, now)?;

        let source_wait = local.retry_after(&source_rate, now);
        let global_wait = global.retry_after(&global_rate, now);
        let retry_after = source_wait.max(global_wait);
        if retry_after > 0 {
            tracing::debug!(
                event = "registration_work_issuance_limited",
                source_bucket = %source_key,
                source_wait,
                global_wait
            );
            return Err(Fault::RateLimited { retry_after }.into());
        }
        local.credit -= 1;
        global.credit -= 1;
        save(&mut self.store, &source_key, &local)?;
        save(&mut self.store, GLOBAL_WORK_ISSUANCE_KEY, &global)
    }
}

const fn work_issuance_rate(rate: &Rate) -> Rate {
    // Abandoned and expired challenges use issuance capacity without creating accounts.
    Rate {
        burst: rate.burst.saturating_mul(WORK_ISSUANCE_HEADROOM),
        refill_seconds: rate.refill_seconds.div_ceil(WORK_ISSUANCE_HEADROOM),
    }
}
