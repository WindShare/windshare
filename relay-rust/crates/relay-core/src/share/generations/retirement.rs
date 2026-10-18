use super::{ConnectionRef, Delivery, Generation, Generations};
use crate::share::{
    Error,
    checkpoint_tracking::Checkpointing,
    deadlines::{Deadlines, Key},
};
use crate::{clock::UnixMillis, flow::Counts};
use alloc::vec::Vec;
use core::time::Duration;

/// The host chooses when a quiescent, unreachable generation can leave admission.
/// No policy supplies a physical-release receipt or an expiry for submitted bytes.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RetirementPolicy {
    /// Native hosts can join their writers and prove release with `Input::Closed`.
    PhysicalRelease,
    /// A fixed grace starts at communication loss, never at reconnect or recovery.
    /// Both limits apply independently; exhaustion leaves the generation admitted.
    Bounded {
        grace: Duration,
        generations: usize,
        residual: Counts,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(in crate::share) enum Detached {
    AwaitingRelease,
    // Destination buffers are gone; queues sourced here may remain elsewhere.
    Released,
    Quiescing { not_before: UnixMillis },
    Retired,
}

impl RetirementPolicy {
    pub(in crate::share) fn detached(self, now: UnixMillis) -> Detached {
        match self {
            Self::PhysicalRelease => Detached::AwaitingRelease,
            Self::Bounded { grace, .. } => Detached::Quiescing {
                not_before: now.saturating_add(grace),
            },
        }
    }

    pub(in crate::share) const fn generations(self) -> usize {
        match self {
            Self::PhysicalRelease => 0,
            Self::Bounded { generations, .. } => generations,
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
    },
    Blocked {
        usage: ConnectionUsage,
        residual: Counts,
        generation_limit: usize,
        residual_limit: Counts,
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
                    state: Detached::Retired,
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

    pub(in crate::share) fn due_retirements(
        &self,
        now: UnixMillis,
        deadlines: &mut Deadlines,
    ) -> Vec<ConnectionRef> {
        let mut due = Vec::new();
        for (&connection, entry) in &self.entries {
            if let Generation::Detached {
                state: Detached::Quiescing { not_before },
                ..
            } = entry
            {
                deadlines.set(
                    Key::Retirement(connection),
                    (now < *not_before).then_some(*not_before),
                );
                if now >= *not_before {
                    due.push(connection);
                }
            }
        }
        due
    }

    /// Cross-connection queue cleanup is established by the share before this call.
    /// The generation owns the remaining timing, risk-budget and persistence rules.
    pub(in crate::share) fn retire(
        &mut self,
        connection: ConnectionRef,
        now: UnixMillis,
        checkpointing: &mut Checkpointing,
    ) -> RetirementProgress {
        let Some(
            entry @ Generation::Detached {
                state: Detached::Quiescing { not_before },
                ..
            },
        ) = self.entries.get(&connection)
        else {
            return RetirementProgress::Waiting;
        };
        if now < *not_before
            || entry
                .ledger()
                .is_some_and(|ledger| !ledger.queues_discarded())
        {
            return RetirementProgress::Waiting;
        }
        let RetirementPolicy::Bounded {
            generations,
            residual: limit,
            ..
        } = self.limits.retirement
        else {
            return RetirementProgress::Waiting;
        };
        let residual = entry.residual_bound();
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
            };
        }
        if let Some(Generation::Detached { state, delivery }) = self.entries.get_mut(&connection) {
            *state = Detached::Retired;
            checkpointing.retention_changed();
            if matches!(delivery, Delivery::Exact(_)) {
                checkpointing.changed(connection);
            }
        }
        RetirementProgress::Retired { residual }
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
            match (*state, self.limits.retirement) {
                (Detached::AwaitingRelease, RetirementPolicy::PhysicalRelease)
                | (Detached::Quiescing { .. }, RetirementPolicy::Bounded { .. }) => {}
                (Detached::Released, _)
                    if entry.residual_bound() == Counts::ZERO
                        && entry
                            .ledger()
                            .is_none_or(|ledger| ledger.metadata_usage().retained_flows == 0) => {}
                (Detached::Retired, RetirementPolicy::Bounded { .. }) => {
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
                _ => return Err(Error::InvalidCheckpoint),
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
