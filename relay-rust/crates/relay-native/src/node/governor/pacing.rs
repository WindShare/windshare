use super::MAX_FRAME;
use std::time::Duration;
use tokio::time::Instant;
use windshare_relay_access::entitlement::Class;

// A delayed wake can span several governor ticks. Leave room for the bytes
// earned during that delay plus any leftover credit smaller than a frame.
const CATCH_UP_WINDOW: Duration = Duration::from_millis(25);
const CREDIT_PER_BYTE: u128 = Duration::from_secs(1).as_nanos();
const FRAME_CREDIT: u128 = MAX_FRAME as u128 * CREDIT_PER_BYTE;

#[derive(Default)]
pub(super) struct Bandwidth {
    pub(super) node: Pacing,
    pub(super) public: Pacing,
    pub(super) preemption: super::preemption::Preemption,
}

#[derive(Clone, Copy)]
pub(super) struct Credit {
    node: u64,
    public: u64,
}

impl Bandwidth {
    pub(super) fn observe_wire(
        &mut self,
        wire: &super::preemption::Egress,
        now: Instant,
    ) -> Option<super::preemption::Transition> {
        let outcome = self.preemption.sample(wire, now);
        self.public.configure(outcome.public_limit, now);
        outcome.transition
    }
    pub(super) fn replenish(&mut self, now: Instant) -> Credit {
        Credit {
            node: self.node.replenish(now),
            public: self.public.replenish(now),
        }
    }

    pub(super) fn charge(&mut self, before: &Credit, after: &Credit) {
        self.node.charge(before.node - after.node);
        self.public.charge(before.public - after.public);
    }
}

impl Credit {
    pub(super) fn reserve(&mut self, class: Class, bytes: u64) -> bool {
        if class == Class::Owner {
            return true;
        }
        if bytes > self.node || bytes > self.public {
            return false;
        }
        self.node -= bytes;
        self.public -= bytes;
        true
    }
}

#[derive(Default)]
pub(super) struct Pacing {
    bucket: Option<Bucket>,
}

struct Bucket {
    bytes_per_second: u64,
    // Byte-nanoseconds preserve fractional bytes across wakes and frame charges.
    credit: u128,
    burst_limit: u128,
    updated: Instant,
}

impl Pacing {
    pub(super) fn capacity(&self) -> Option<u64> {
        self.bucket.as_ref().map(|bucket| bucket.bytes_per_second)
    }

    pub(super) fn configure(&mut self, bytes_per_second: Option<u64>, now: Instant) -> bool {
        if bytes_per_second == self.capacity() {
            return false;
        }

        self.bucket = bytes_per_second.map(|rate| Bucket {
            bytes_per_second: rate,
            // An administrative rate change discards catch-up credit at the old rate.
            credit: self
                .bucket
                .as_ref()
                .map_or(FRAME_CREDIT, |bucket| bucket.credit.min(FRAME_CREDIT)),
            burst_limit: FRAME_CREDIT + u128::from(rate) * CATCH_UP_WINDOW.as_nanos(),
            updated: now,
        });
        true
    }

    pub(super) fn replenish(&mut self, now: Instant) -> u64 {
        let Some(bucket) = &mut self.bucket else {
            return u64::MAX;
        };

        let elapsed = now.duration_since(bucket.updated);
        let earned = elapsed
            .as_nanos()
            .saturating_mul(u128::from(bucket.bytes_per_second));
        bucket.credit = bucket.credit.saturating_add(earned).min(bucket.burst_limit);
        bucket.updated = now;
        u64::try_from(bucket.credit / CREDIT_PER_BYTE).expect("burst limit fits in bytes")
    }

    pub(super) fn charge(&mut self, bytes: u64) {
        if let Some(bucket) = &mut self.bucket {
            bucket.credit -= u128::from(bytes) * CREDIT_PER_BYTE;
        }
    }
}

#[cfg(test)]
#[path = "pacing_tests.rs"]
mod tests;
