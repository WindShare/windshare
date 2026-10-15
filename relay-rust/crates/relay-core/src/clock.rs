//! Host-read wall-clock time: the single clock every engine decision uses.

use core::time::Duration;

const MILLIS_PER_SECOND: u64 = 1_000;

/// Milliseconds since the Unix epoch, as read from the host clock.
///
/// Deadlines are absolute wall-clock instants rather than offsets from a
/// process-local monotonic clock, so a host that hibernates or restarts can
/// persist them and re-check them on wake without extending any budget.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct UnixMillis(pub u64);

impl UnixMillis {
    /// Returns the instant `duration` later.
    ///
    /// Saturates rather than wraps, so overflow can never move a deadline
    /// into the past and expire it early.
    #[must_use]
    pub fn saturating_add(self, duration: Duration) -> Self {
        let millis = u64::try_from(duration.as_millis()).unwrap_or(u64::MAX);
        Self(self.0.saturating_add(millis))
    }

    /// Returns the first whole second at or after this instant.
    #[must_use]
    pub const fn ceil_seconds(self) -> UnixSeconds {
        UnixSeconds(self.0.div_ceil(MILLIS_PER_SECOND))
    }
}

/// Seconds since the Unix epoch: the precision the wire carries.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub struct UnixSeconds(pub u64);

impl UnixSeconds {
    /// Returns the first millisecond of this second.
    #[must_use]
    pub const fn start(self) -> UnixMillis {
        UnixMillis(self.0.saturating_mul(MILLIS_PER_SECOND))
    }
}

#[cfg(test)]
#[path = "clock_tests.rs"]
mod tests;
