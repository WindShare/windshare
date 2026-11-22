//! Short control lifetimes never reserve payload capacity or business admission.
use super::{ConnectionRef, Error, Observer, RelayDecision, ShareRelay};
use crate::clock::UnixMillis;
use core::{num::NonZeroU64, time::Duration};

pub const CONTROL_LIFETIME: Duration = Duration::from_secs(30);

#[derive(Clone, Copy, Debug)]
pub struct HandshakeLimits {
    /// First-frame and STOP sockets share this bounded control reserve.
    pub concurrent: usize,
    /// Applies to new connections; promotion and closure never refund attempts.
    pub attempts: AttemptRate,
}

/// One token is replenished per interval; rejected or closed attempts are not refunded.
#[derive(Clone, Copy, Debug)]
pub struct AttemptRate {
    pub burst: u32,
    pub refill: Duration,
}

#[derive(Clone, Copy, Debug)]
pub struct AttemptLimiter {
    burst: u32,
    refill_millis: NonZeroU64,
    tokens: u32,
    updated: UnixMillis,
}

impl AttemptLimiter {
    /// # Errors
    /// Refuses zero or unrepresentable rate bounds.
    pub fn new(rate: AttemptRate, now: UnixMillis) -> Result<Self, Error> {
        let refill_millis = u64::try_from(rate.refill.as_millis())
            .ok()
            .and_then(NonZeroU64::new)
            .ok_or(Error::Configuration)?;
        if rate.burst == 0 {
            return Err(Error::Configuration);
        }
        Ok(Self {
            burst: rate.burst,
            refill_millis,
            tokens: rate.burst,
            updated: now,
        })
    }

    pub const fn burst(&self) -> u32 {
        self.burst
    }

    /// A backwards clock neither replenishes tokens nor moves the refill origin.
    pub fn attempt(&mut self, now: UnixMillis) -> bool {
        self.attempt_with_reserve(now, 0)
    }

    /// Keeps at least `reserve` tokens untouched, preserving burst headroom
    /// and subsequent refills for higher-priority callers.
    pub fn attempt_with_reserve(&mut self, now: UnixMillis, reserve: u32) -> bool {
        let interval = self.refill_millis.get();
        let elapsed = now.0.saturating_sub(self.updated.0);
        let refill = elapsed / interval;
        if refill != 0 {
            self.tokens = self
                .tokens
                .saturating_add(u32::try_from(refill).unwrap_or(u32::MAX))
                .min(self.burst);
            self.updated.0 = now.0 - elapsed % interval;
        }
        if self.tokens <= reserve {
            return false;
        }
        self.tokens -= 1;
        true
    }
}

impl<O: Observer> ShareRelay<O> {
    pub(super) fn admit_business(
        &mut self,
        connection: ConnectionRef,
        now: UnixMillis,
    ) -> Result<(), Error> {
        if let Err(error) = self.generations.admit_business(connection) {
            if error == Error::Capacity {
                self.trace(
                    Some(connection),
                    RelayDecision::BusinessCapacity {
                        usage: self.connection_usage(),
                    },
                    now,
                );
            }
            return Err(error);
        }
        self.trace(Some(connection), RelayDecision::BusinessAdmitted, now);
        Ok(())
    }
}
