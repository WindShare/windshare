use crate::cbor::{Decoder, Encoder};
use serde::{Deserialize, Serialize};

pub const DAY_SECONDS: u64 = 86_400;
pub const ENTITLEMENT_LIFETIME: u64 = 5 * 60;
pub const DEFAULT_DAILY_BYTES: u64 = 10 * 1024 * 1024 * 1024;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct Entitlement {
    pub version: u64,
    /// UTC day number, independent of report arrival time.
    pub day: u64,
    pub valid_until: u64,
    pub remaining_bytes: u64,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Class {
    Lowest,
    Standard,
    Owner,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Allowance {
    Available,
    Spent,
    Stale,
}

impl Entitlement {
    pub const fn allowance(self, now: u64) -> Allowance {
        if self.day != now / DAY_SECONDS || now >= self.valid_until {
            Allowance::Stale
        } else if self.remaining_bytes == 0 {
            Allowance::Spent
        } else {
            Allowance::Available
        }
    }

    pub const fn class(allowance: Allowance) -> Class {
        match allowance {
            Allowance::Available => Class::Standard,
            Allowance::Spent | Allowance::Stale => Class::Lowest,
        }
    }

    pub const fn valid_snapshot(self, issued_at: u64) -> bool {
        self.version > 0
            && self.day == issued_at / DAY_SECONDS
            && self.valid_until > issued_at
            && self.valid_until <= issued_at.saturating_add(ENTITLEMENT_LIFETIME)
            && self.valid_until <= (self.day + 1).saturating_mul(DAY_SECONDS)
    }

    pub(crate) fn encode(self, out: &mut Encoder) {
        out.array(4);
        out.uint(self.version);
        out.uint(self.day);
        out.uint(self.valid_until);
        out.uint(self.remaining_bytes);
    }

    pub(crate) fn decode(input: &mut Decoder<'_>) -> Result<Self, crate::trust::Error> {
        input.array(4)?;
        Ok(Self {
            version: input.uint()?,
            day: input.uint()?,
            valid_until: input.uint()?,
            remaining_bytes: input.uint()?,
        })
    }
}

/// Equality is not a refresh. Old tickets cannot extend a newer snapshot's lifetime.
pub fn advance(current: &mut Option<Entitlement>, next: Entitlement) -> bool {
    if current.is_some_and(|value| next.version <= value.version || next.day < value.day) {
        return false;
    }
    *current = Some(next);
    true
}
