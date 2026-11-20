use crate::{
    Authority, Challenge, Environment, Fault, Rate, RegistrationMode, Settings, Store,
    challenges::{AcceptedProof, CHALLENGE_SECONDS, ProofCheck, ProofKind, VerifiedEmail},
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

#[derive(Clone, Copy)]
pub struct AdmissionProof<'a> {
    pub challenge: Option<&'a str>,
    pub solution: Option<&'a str>,
}

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

pub struct CreationAdmission {
    source_key: String,
    source: SourceCreation,
    global: TokenBucket,
    mode: RegistrationMode,
    proof: Option<AcceptedProof>,
}

impl CreationAdmission {
    pub(crate) const fn mode(&self) -> RegistrationMode {
        self.mode
    }

    pub(crate) fn source_bucket(&self) -> &str {
        &self.source_key
    }

    pub(crate) const fn verified_email(&self) -> Option<(&str, &str)> {
        match &self.proof {
            Some(proof) => proof.verified_email(),
            None => None,
        }
    }

    pub(crate) fn consume(mut self, store: &mut impl Store) -> Result<Option<VerifiedEmail>> {
        self.source.bucket.credit -= 1;
        self.global.credit -= 1;
        save(store, &self.source_key, &self.source)?;
        save(store, GLOBAL_CREATION_KEY, &self.global)?;
        self.proof
            .map(|proof| proof.consume(store))
            .transpose()
            .map(Option::flatten)
    }
}

pub fn throttle_key(scope: &str, identity: &[u8]) -> String {
    key(&format!("throttle/{scope}"), &Sha256::digest(identity))
}

impl<R: Store, E: Environment> Authority<R, E> {
    pub(crate) fn check_throttle(&self, scope: &str, identity: &[u8], rate: &Rate) -> Result<()> {
        let now = self.environment.now();
        let bucket = TokenBucket::load(&self.store, &throttle_key(scope, identity), rate, now)?;
        let retry_after = bucket.retry_after(rate, now);
        if retry_after > 0 {
            tracing::debug!(event = "authority_throttled", scope, retry_after);
            return Err(Fault::RateLimited { retry_after }.into());
        }
        Ok(())
    }

    pub(crate) fn throttle(
        &mut self,
        source: &str,
        scope: &str,
        burst: u64,
        refill_seconds: u64,
    ) -> Result<()> {
        self.throttle_all([(
            scope,
            throttle_key(scope, source.as_bytes()),
            Rate {
                burst,
                refill_seconds,
            },
        )])
    }

    pub(crate) fn throttle_all<const N: usize>(
        &mut self,
        limits: [(&str, String, Rate); N],
    ) -> Result<()> {
        let now = self.environment.now();
        let mut buckets = Vec::with_capacity(N);
        let mut retry_after = 0;
        let mut blocking_scope = "";
        for (scope, key, rate) in limits {
            let bucket = TokenBucket::load(&self.store, &key, &rate, now)?;
            let wait = bucket.retry_after(&rate, now);
            if wait > retry_after {
                retry_after = wait;
                blocking_scope = scope;
            }
            buckets.push((key, bucket));
        }
        if retry_after > 0 {
            tracing::debug!(
                event = "authority_throttled",
                scope = blocking_scope,
                retry_after
            );
            return Err(Fault::RateLimited { retry_after }.into());
        }
        // Source, account and global limits are independent; spend from all only
        // when every bucket admits the operation so global backpressure never
        // drains an innocent caller's local budget.
        for (key, mut bucket) in buckets {
            bucket.credit -= 1;
            save(&mut self.store, &key, &bucket)?;
        }
        Ok(())
    }

    pub(crate) fn creation_admission(
        &mut self,
        source: &str,
        attempt: RegistrationAttempt,
        proof: AdmissionProof<'_>,
        settings: &Settings,
        account_count: u64,
    ) -> Result<CreationAdmission> {
        let now = self.environment.now();
        let source_id: [u8; 32] = Sha256::digest(source.as_bytes()).into();
        let source_key = key("creation-source", &source_id);
        let mut local =
            load::<SourceCreation>(&self.store, &source_key)?.unwrap_or_else(|| SourceCreation {
                bucket: TokenBucket::new(&settings.source_rate, now),
                bursts: 0,
            });
        let global =
            TokenBucket::load(&self.store, GLOBAL_CREATION_KEY, &settings.global_rate, now)?;
        if local.bucket.refill(&settings.source_rate, now)
            && local.bucket.credit == settings.source_rate.burst
        {
            local.bursts = 0;
        }
        if local.bucket.credit == 0 || global.credit == 0 {
            let reason = creation_rate_reason(local.bucket.credit == 0, global.credit == 0);
            let retry_after = settings
                .source_rate
                .refill_seconds
                .max(settings.global_rate.refill_seconds);
            local.bursts = local.bursts.saturating_add(1);
            save(&mut self.store, &source_key, &local)?;
            tracing::debug!(
                event = "registration_rate_limited",
                attempt = %attempt.fingerprint(),
                source_bucket = %source_key,
                reason,
                bursts = local.bursts,
                retry_after,
            );
            return Err(Fault::RateLimited { retry_after }.into());
        }
        let policy_mode = settings.registration.mode(
            account_count,
            self.environment.registration_email_configured(),
        );
        let escalated =
            policy_mode == RegistrationMode::Automatic && local.bursts >= SOURCE_BURSTS_FOR_WORK;
        let mode = if escalated {
            RegistrationMode::ProofOfWork
        } else {
            policy_mode
        };
        let proof = match mode {
            RegistrationMode::Automatic => None,
            RegistrationMode::ProofOfWork | RegistrationMode::Email => {
                let kind = if mode == RegistrationMode::Email {
                    ProofKind::Email
                } else {
                    ProofKind::Work
                };
                match self.check_registration_proof(
                    attempt,
                    kind,
                    proof.challenge,
                    proof.solution,
                )? {
                    ProofCheck::Accepted(proof) => Some(proof),
                    ProofCheck::Retry(challenge) if kind == ProofKind::Email => {
                        return Err(verification_required(
                            attempt,
                            &source_key,
                            mode,
                            escalated,
                            challenge,
                        ));
                    }
                    ProofCheck::Retry(_) | ProofCheck::Unavailable => {
                        let challenge = match kind {
                            ProofKind::Email => Challenge {
                                id: String::new(),
                                mode,
                                bits: 0,
                                expires_at: now + CHALLENGE_SECONDS,
                            },
                            ProofKind::Work => {
                                self.work_challenge(attempt, &source_id, settings)?
                            }
                        };
                        return Err(verification_required(
                            attempt,
                            &source_key,
                            mode,
                            escalated,
                            challenge,
                        ));
                    }
                }
            }
        };
        Ok(CreationAdmission {
            source_key,
            source: local,
            global,
            mode,
            proof,
        })
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

const fn creation_rate_reason(local_empty: bool, global_empty: bool) -> &'static str {
    match (local_empty, global_empty) {
        (true, true) => "source_and_global_rate_limited",
        (true, false) => "source_rate_limited",
        (false, _) => "global_rate_limited",
    }
}

fn verification_required(
    attempt: RegistrationAttempt,
    source_bucket: &str,
    mode: RegistrationMode,
    escalated: bool,
    challenge: Challenge,
) -> anyhow::Error {
    tracing::debug!(
        event = "registration_verification_required",
        attempt = %attempt.fingerprint(),
        source_bucket,
        ?mode,
        reason = if escalated { "source_burst" } else { "policy" },
    );
    Fault::Verification { challenge }.into()
}

const fn work_issuance_rate(rate: &Rate) -> Rate {
    // Abandoned and expired challenges use issuance capacity without creating accounts.
    Rate {
        burst: rate.burst.saturating_mul(WORK_ISSUANCE_HEADROOM),
        refill_seconds: rate.refill_seconds.div_ceil(WORK_ISSUANCE_HEADROOM),
    }
}
