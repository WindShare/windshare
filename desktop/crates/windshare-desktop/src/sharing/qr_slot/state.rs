use crate::motion::whirl::{Wind, bloom::BLOOM, seconds_between};
use std::time::{Duration, Instant};

pub(super) const FADE: f32 = 0.44;

/// An inherited whirl keeps its clock; a dialog without one raises its own.
#[derive(Clone, Copy, Debug)]
pub enum Arrival {
    Whirl(Option<Wind>),
    Fade,
}

#[derive(Clone, Copy, Debug)]
pub(super) struct Signal {
    pub code: bool,
    pub pending: bool,
    pub still: bool,
    pub arrival: Arrival,
}

#[derive(Clone, Copy, Debug, PartialEq)]
pub(super) enum Phase {
    Gathering {
        wind: Wind,
    },
    Waiting,
    Blooming {
        wind: Wind,
        since: Instant,
    },
    Fading {
        since: Instant,
    },
    /// A code present on mount has no completion halo.
    Settled {
        bloomed: Option<Instant>,
    },
    Unavailable,
}

impl Phase {
    pub fn enter(signal: Signal, now: Instant) -> Self {
        if signal.code {
            Self::Settled { bloomed: None }
        } else if signal.pending {
            match signal.arrival {
                Arrival::Whirl(wind) => Self::Gathering {
                    wind: wind.unwrap_or(Wind::Rooted { since: now }),
                },
                Arrival::Fade => Self::Waiting,
            }
        } else {
            Self::Unavailable
        }
    }

    pub fn advance(self, signal: Signal, now: Instant) -> Self {
        if signal.still && signal.code {
            return Self::Settled { bloomed: None };
        }
        match self {
            // A code can only bloom out of a whirl that already stands here.
            Self::Gathering { wind } if signal.code => {
                if wind.standing(now).is_some() {
                    Self::Blooming { wind, since: now }
                } else {
                    self
                }
            }
            Self::Waiting if signal.code => Self::Fading { since: now },
            Self::Gathering { .. } | Self::Waiting if !signal.code && signal.pending => self,
            Self::Blooming { since, .. } if signal.code => {
                if seconds_between(since, now) < BLOOM {
                    self
                } else {
                    Self::Settled {
                        bloomed: Some(since + Duration::from_secs_f32(BLOOM)),
                    }
                }
            }
            Self::Fading { since } if signal.code => {
                if seconds_between(since, now) < FADE {
                    self
                } else {
                    Self::Settled { bloomed: None }
                }
            }
            Self::Settled { .. } if signal.code => self,
            Self::Unavailable if !signal.code && !signal.pending => self,
            _ => Self::enter(signal, now),
        }
    }
}

#[cfg(test)]
#[path = "../qr_slot_tests.rs"]
mod tests;
