use super::{ConnectionRef, Delivery, Generation, Generations};
use crate::share::{Error, deadlines::Key};
use crate::{clock::UnixMillis, flow::Counts};
use alloc::vec::Vec;
use core::time::Duration;

/// The host bounds admission risk after loss, independently of physical release.
/// Expiry ends a reservation; it never acknowledges consumption or freed buffers.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RetirementPolicy {
    /// Native hosts can join their writers and prove release with `Input::Closed`.
    PhysicalRelease,
    /// Both intervals start at the first observed loss, never at retirement or recovery.
    /// Risk retention must cover admission grace. Cleanup remains mandatory even
    /// after expiry; capacity exhaustion cannot postpone the original expiry.
    Bounded {
        admission_grace: Duration,
        risk_retention: Duration,
        generations: usize,
        residual: Counts,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(in crate::share) enum Detached {
    AwaitingRelease,
    // Destination buffers are gone; queues sourced here may remain elsewhere.
    Released,
    Quiescing {
        retire_at: UnixMillis,
        risk_expires_at: UnixMillis,
    },
    RiskRetained {
        expires_at: UnixMillis,
    },
}

impl RetirementPolicy {
    pub(in crate::share) fn detached(self, now: UnixMillis) -> Detached {
        match self {
            Self::PhysicalRelease => Detached::AwaitingRelease,
            Self::Bounded {
                admission_grace,
                risk_retention,
                ..
            } => Detached::Quiescing {
                retire_at: now.saturating_add(admission_grace),
                risk_expires_at: now.saturating_add(risk_retention),
            },
        }
    }

    pub(in crate::share) const fn generations(self) -> usize {
        match self {
            Self::PhysicalRelease => 0,
            Self::Bounded { generations, .. } => generations,
        }
    }

    pub(in crate::share) fn validate(self) -> Result<(), Error> {
        if let Self::Bounded {
            admission_grace,
            risk_retention,
            ..
        } = self
            && risk_retention < admission_grace
        {
            return Err(Error::Configuration);
        }
        Ok(())
    }
}

impl Detached {
    pub(super) const fn deadline(self) -> Option<UnixMillis> {
        match self {
            Self::Quiescing { retire_at, .. } => Some(retire_at),
            Self::RiskRetained { expires_at } => Some(expires_at),
            Self::AwaitingRelease | Self::Released => None,
        }
    }

    pub(in crate::share) fn validate(self, policy: RetirementPolicy) -> Result<(), Error> {
        match (self, policy) {
            (Self::Released, _)
            | (Self::AwaitingRelease, RetirementPolicy::PhysicalRelease)
            | (Self::RiskRetained { .. }, RetirementPolicy::Bounded { .. }) => Ok(()),
            (
                Self::Quiescing {
                    retire_at,
                    risk_expires_at,
                },
                RetirementPolicy::Bounded { .. },
            ) if retire_at <= risk_expires_at => Ok(()),
            _ => Err(Error::InvalidCheckpoint),
        }
    }
}

/// Business includes communicating data sockets and unfinished delivery retirement.
/// Residual counts cover retired generations only; live delivery keeps its own budget.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct ConnectionUsage {
    pub business: usize,
    pub controls: usize,
    pub retired_generations: usize,
    pub residual: Counts,
    /// Conservative reservations across admitted and retired unavailable generations.
    pub uncertain: Counts,
}

pub(in crate::share) enum RetirementProgress {
    Waiting,
    Retired {
        residual: Counts,
        expires_at: UnixMillis,
    },
    RiskExpired {
        residual: Counts,
        expires_at: UnixMillis,
    },
    Blocked {
        usage: ConnectionUsage,
        residual: Counts,
        generation_limit: usize,
        residual_limit: Counts,
        expires_at: UnixMillis,
    },
}

impl Generation {
    fn residual_bound(&self) -> Counts {
        match self {
            Self::Control { .. } => Counts::ZERO,
            Self::Business { ledger, .. }
            | Self::Detached {
                delivery: Delivery::Exact(ledger),
                ..
            } => ledger.usage().submitted,
            Self::Detached {
                delivery: Delivery::Unavailable(bound),
                ..
            } => *bound,
        }
    }
}

impl Generations {
    pub(in crate::share) fn usage(&self) -> ConnectionUsage {
        let mut usage = ConnectionUsage::default();
        for entry in self.entries.values() {
            if let Generation::Detached {
                delivery: Delivery::Unavailable(bound),
                ..
            } = entry
            {
                usage.uncertain.frames = usage.uncertain.frames.saturating_add(bound.frames);
                usage.uncertain.encoded_bytes = usage
                    .uncertain
                    .encoded_bytes
                    .saturating_add(bound.encoded_bytes);
            }
            match entry {
                Generation::Control { .. } => usage.controls += 1,
                Generation::Detached {
                    state: Detached::RiskRetained { .. },
                    ..
                } => {
                    usage.retired_generations += 1;
                    let bound = entry.residual_bound();
                    // Retirement and recovery validate this aggregate before admission.
                    usage.residual.frames += bound.frames;
                    usage.residual.encoded_bytes += bound.encoded_bytes;
                }
                Generation::Business { .. } | Generation::Detached { .. } => usage.business += 1,
            }
        }
        usage
    }

    pub(in crate::share) fn unavailable_reservation(
        &self,
        connection: ConnectionRef,
    ) -> Option<Counts> {
        match self.entries.get(&connection)? {
            Generation::Detached {
                delivery: Delivery::Unavailable(bound),
                ..
            } => Some(*bound),
            _ => None,
        }
    }

    pub(in crate::share) fn retirement_state(
        &self,
        connection: &ConnectionRef,
    ) -> Option<Detached> {
        match self.entries.get(connection)? {
            Generation::Detached { state, .. } => Some(*state),
            _ => None,
        }
    }

    pub(in crate::share) fn released(&self) -> impl Iterator<Item = ConnectionRef> + '_ {
        self.entries.iter().filter_map(|(&id, entry)| {
            matches!(
                entry,
                Generation::Detached {
                    state: Detached::Released,
                    ..
                }
            )
            .then_some(id)
        })
    }

    pub(in crate::share) fn due_retirements(&mut self, now: UnixMillis) -> Vec<ConnectionRef> {
        let mut expiring = Vec::new();
        let mut retiring = Vec::new();
        for (&connection, entry) in &self.entries {
            let (retire_at, expires_at) = match entry {
                Generation::Detached {
                    state:
                        Detached::Quiescing {
                            retire_at,
                            risk_expires_at,
                        },
                    ..
                } => (Some(*retire_at), *risk_expires_at),
                Generation::Detached {
                    state: Detached::RiskRetained { expires_at },
                    ..
                } => (None, *expires_at),
                _ => continue,
            };
            let next = retire_at
                .filter(|at| now < *at)
                .or_else(|| (now < expires_at).then_some(expires_at));
            self.deadlines.set(Key::Retirement(connection), next);
            if now >= expires_at {
                expiring.push(connection);
            } else if retire_at.is_some_and(|at| now >= at) {
                retiring.push(connection);
            }
        }
        // Free expired reservations before retrying blocked retirements, regardless
        // of connection-ID order. One alarm must suffice to restore admission.
        expiring.extend(retiring);
        expiring
    }

    /// Cross-connection queue cleanup is established by the share before this call.
    /// The generation owns the remaining timing, risk-budget and persistence rules.
    pub(in crate::share) fn progress_retirement(
        &mut self,
        connection: ConnectionRef,
        now: UnixMillis,
    ) -> RetirementProgress {
        let Some(entry @ Generation::Detached { state, .. }) = self.entries.get(&connection) else {
            return RetirementProgress::Waiting;
        };
        let (retire_at, expires_at) = match *state {
            Detached::Quiescing {
                retire_at,
                risk_expires_at,
            } => (retire_at, risk_expires_at),
            Detached::RiskRetained { expires_at } => (expires_at, expires_at),
            _ => return RetirementProgress::Waiting,
        };
        if now < retire_at
            || entry
                .ledger()
                .is_some_and(|ledger| !ledger.queues_discarded())
        {
            return RetirementProgress::Waiting;
        }
        let residual = entry.residual_bound();
        if now >= expires_at {
            // Drop only the unreachable destination's accounting. Do not settle
            // its flows or refund debt submitted to a still-communicating peer.
            self.remove_detached(connection);
            return RetirementProgress::RiskExpired {
                residual,
                expires_at,
            };
        }
        let RetirementPolicy::Bounded {
            generations,
            residual: limit,
            ..
        } = self.limits.retirement
        else {
            return RetirementProgress::Waiting;
        };
        let usage = self.usage();
        if residual != Counts::ZERO
            && (usage.retired_generations >= generations
                || residual.frames > limit.frames.saturating_sub(usage.residual.frames)
                || residual.encoded_bytes
                    > limit
                        .encoded_bytes
                        .saturating_sub(usage.residual.encoded_bytes))
        {
            return RetirementProgress::Blocked {
                usage,
                residual,
                generation_limit: generations,
                residual_limit: limit,
                expires_at,
            };
        }
        if let Some(Generation::Detached { state, delivery }) = self.entries.get_mut(&connection) {
            *state = Detached::RiskRetained { expires_at };
            self.changes.retention_changed();
            if matches!(delivery, Delivery::Exact(_)) {
                self.changes.changed(connection);
            }
        }
        self.deadlines
            .set(Key::Retirement(connection), Some(expires_at));
        RetirementProgress::Retired {
            residual,
            expires_at,
        }
    }

    pub(in crate::share) fn validate_retirements(
        &self,
        mut quiescent: impl FnMut(ConnectionRef) -> bool,
    ) -> Result<(), Error> {
        let mut retained = 0;
        let mut residual = Counts::ZERO;
        for (&connection, entry) in &self.entries {
            let Generation::Detached { state, .. } = entry else {
                continue;
            };
            state.validate(self.limits.retirement)?;
            match *state {
                Detached::AwaitingRelease | Detached::Quiescing { .. } => {}
                Detached::Released
                    if entry.residual_bound() == Counts::ZERO
                        && entry
                            .ledger()
                            .is_none_or(|ledger| ledger.metadata_usage().retained_flows == 0) => {}
                Detached::RiskRetained { .. } => {
                    if !quiescent(connection) {
                        return Err(Error::InvalidCheckpoint);
                    }
                    retained += 1;
                    let bound = entry.residual_bound();
                    residual.frames = residual
                        .frames
                        .checked_add(bound.frames)
                        .ok_or(Error::InvalidCheckpoint)?;
                    residual.encoded_bytes = residual
                        .encoded_bytes
                        .checked_add(bound.encoded_bytes)
                        .ok_or(Error::InvalidCheckpoint)?;
                }
                Detached::Released => return Err(Error::InvalidCheckpoint),
            }
        }
        if let RetirementPolicy::Bounded {
            generations,
            residual: limit,
            ..
        } = self.limits.retirement
            && (retained > generations
                || residual.frames > limit.frames
                || residual.encoded_bytes > limit.encoded_bytes)
        {
            return Err(Error::InvalidCheckpoint);
        }
        Ok(())
    }
}
