//! Two independent resource dimensions; neither a write nor cancellation is consumption.

use core::time::Duration;

use crate::sessions::OpaqueHeader;

/// Frame counts and full encoded WS2O bytes, never plaintext or ciphertext alone.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct Counts {
    /// Number of complete envelopes.
    pub frames: u64,
    /// Includes each envelope's routing header.
    pub encoded_bytes: u64,
}

impl Counts {
    /// No resource occupancy.
    pub const ZERO: Self = Self::new(0, 0);

    /// The dimensions are independent, including frame-only and byte-only grants.
    #[must_use]
    pub const fn new(frames: u64, encoded_bytes: u64) -> Self {
        Self {
            frames,
            encoded_bytes,
        }
    }

    pub(super) const fn is_envelope_batch(self) -> bool {
        // Divide instead of multiplying so malformed cumulative u64 counts cannot
        // overflow validation. Zero frames must describe exactly zero bytes.
        self.frames <= self.encoded_bytes / OpaqueHeader::MIN_ENCODED_BYTES as u64
            && self
                .encoded_bytes
                .div_ceil(OpaqueHeader::MAX_ENCODED_BYTES as u64)
                <= self.frames
    }

    pub(super) const fn covers(self, other: Self) -> bool {
        self.frames >= other.frames && self.encoded_bytes >= other.encoded_bytes
    }

    pub(super) const fn min(self, other: Self) -> Self {
        Self::new(
            if self.frames < other.frames {
                self.frames
            } else {
                other.frames
            },
            if self.encoded_bytes < other.encoded_bytes {
                self.encoded_bytes
            } else {
                other.encoded_bytes
            },
        )
    }

    pub(super) fn checked_add(self, other: Self) -> Option<Self> {
        Some(Self::new(
            self.frames.checked_add(other.frames)?,
            self.encoded_bytes.checked_add(other.encoded_bytes)?,
        ))
    }

    pub(super) fn add(self, other: Self) -> Self {
        self.checked_add(other).expect("bounded resource sum")
    }

    pub(super) const fn sub(self, other: Self) -> Self {
        Self::new(
            self.frames
                .checked_sub(other.frames)
                .expect("accounted frames"),
            self.encoded_bytes
                .checked_sub(other.encoded_bytes)
                .expect("accounted bytes"),
        )
    }
}

/// Occupancy stays charged while moving between these three stages.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct Usage {
    /// Credit issued to sources but not received by the relay.
    pub granted: Counts,
    /// Payload accepted by the relay but not submitted to its destination host.
    pub received: Counts,
    /// Payload submitted to the host but not consumed by its destination.
    pub submitted: Counts,
}

impl Usage {
    /// Total outstanding responsibility of the destination connection.
    #[must_use]
    pub fn total(self) -> Counts {
        self.granted.add(self.received).add(self.submitted)
    }

    pub(super) fn replace(self, before: Self, after: Self) -> Self {
        Self {
            granted: self.granted.sub(before.granted).add(after.granted),
            received: self.received.sub(before.received).add(after.received),
            submitted: self.submitted.sub(before.submitted).add(after.submitted),
        }
    }
}

/// Host-selected capacities. No platform defaults or scheduling policy live here.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Limits {
    pub(super) total: Counts,
    pub(super) startup_reserve: Counts,
    pub(super) startup_grant: Counts,
    pub(super) settlement_timeout: Duration,
}

/// Invalid capacity configuration, rejected before a connection can issue credit.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ConfigurationError {
    /// Startup must allow at least one maximum-sized opaque handshake envelope.
    StartupTooSmall,
    /// Startup grants must fit their pool, and that pool must fit the total.
    CapacityOrder,
    /// Unsettled resources require a finite, positive timeout.
    EmptyTimeout,
    /// The host supplied no bounded flow storage.
    EmptyStorage,
}

impl Limits {
    /// Reserves a separate startup pool inside the connection's total capacity.
    ///
    /// Only a new flow's initial grant can use that pool; it can also use spare
    /// ordinary capacity. All subsequent grants use the ordinary pool, so busy
    /// existing flows cannot spend future startup capacity. Consumption does not
    /// replenish the initial grant automatically.
    ///
    /// # Errors
    ///
    /// Rejects insufficient handshake capacity, inconsistent bounds or zero timeout.
    pub const fn new(
        total: Counts,
        startup_reserve: Counts,
        startup_grant: Counts,
        settlement_timeout: Duration,
    ) -> Result<Self, ConfigurationError> {
        if startup_grant.frames == 0
            || startup_grant.encoded_bytes < OpaqueHeader::MAX_ENCODED_BYTES as u64
        {
            return Err(ConfigurationError::StartupTooSmall);
        }
        if !total.covers(startup_reserve) || !startup_reserve.covers(startup_grant) {
            return Err(ConfigurationError::CapacityOrder);
        }
        if settlement_timeout.is_zero() {
            return Err(ConfigurationError::EmptyTimeout);
        }
        Ok(Self {
            total,
            startup_reserve,
            startup_grant,
            settlement_timeout,
        })
    }

    /// Total bound, including startup reservations and all three occupancy stages.
    #[must_use]
    pub const fn total(self) -> Counts {
        self.total
    }

    /// Capacity inaccessible to ordinary replenishment.
    #[must_use]
    pub const fn startup_reserve(self) -> Counts {
        self.startup_reserve
    }

    /// Exactly one initial grant per flow; it does not wait for the first frame.
    #[must_use]
    pub const fn startup_grant(self) -> Counts {
        self.startup_grant
    }

    pub(super) const fn regular(self) -> Counts {
        self.total.sub(self.startup_reserve)
    }
}
