//! Admission lifetime is separate from the host's unresolved delivery responsibility.
use super::{ConnectionRef, Error, Key, Limits, Observer, RelayDecision, ShareRelay};
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
pub(super) enum Detached {
    AwaitingRelease,
    // Destination buffers are gone; queues sourced here may remain elsewhere.
    Released,
    Quiescing { not_before: UnixMillis },
    Retired,
}

impl RetirementPolicy {
    pub(super) fn detached(self, now: UnixMillis) -> Detached {
        match self {
            Self::PhysicalRelease => Detached::AwaitingRelease,
            Self::Bounded { grace, .. } => Detached::Quiescing {
                not_before: now.saturating_add(grace),
            },
        }
    }

    pub(super) const fn generations(self) -> usize {
        match self {
            Self::PhysicalRelease => 0,
            Self::Bounded { generations, .. } => generations,
        }
    }
}

/// Admitted includes handshakes, communicating sockets and unfinished retirement.
/// Residual counts cover retired generations only; live delivery keeps its own budget.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct ConnectionUsage {
    pub admitted: usize,
    pub retired_generations: usize,
    pub residual: Counts,
    /// Conservative reservations across admitted and retired unavailable generations.
    pub uncertain: Counts,
}

impl Limits {
    pub(super) fn generation_limit(self) -> Result<usize, Error> {
        self.connections
            .checked_add(self.retirement.generations())
            .ok_or(Error::Configuration)
    }
}

impl<O: Observer> ShareRelay<O> {
    #[must_use]
    pub fn connection_usage(&self) -> ConnectionUsage {
        let mut usage = ConnectionUsage::default();
        for bound in self.unavailable.values() {
            usage.uncertain.frames = usage.uncertain.frames.saturating_add(bound.frames);
            usage.uncertain.encoded_bytes = usage
                .uncertain
                .encoded_bytes
                .saturating_add(bound.encoded_bytes);
        }
        for (&connection, state) in &self.detached {
            if *state == Detached::Retired {
                usage.retired_generations += 1;
                let residual = self.residual_bound(connection);
                // Each transition checked the sum against the injected risk budget.
                usage.residual.frames += residual.frames;
                usage.residual.encoded_bytes += residual.encoded_bytes;
            }
        }
        usage.admitted = self.ledgers.len() + self.unavailable.len() - usage.retired_generations;
        usage
    }

    /// None means exact delivery accounting is available or no responsibility remains.
    #[must_use]
    pub fn unavailable_reservation(&self, connection: ConnectionRef) -> Option<Counts> {
        self.unavailable.get(&connection).copied()
    }

    pub(super) fn residual_bound(&self, connection: ConnectionRef) -> Counts {
        self.unavailable
            .get(&connection)
            .copied()
            .unwrap_or_else(|| self.ledgers[&connection].usage().submitted)
    }

    pub(super) fn generation_quiescent(&self, connection: ConnectionRef) -> bool {
        // The lost peer can also be the source of queued payloads on a healthy
        // destination. Its admission slot must not hide those application queues.
        self.routes.values().all(|flow| {
            (flow.destination() != connection && flow.permit().source() != connection)
                || self
                    .ledgers
                    .get(&flow.destination())
                    .and_then(|ledger| ledger.snapshot(*flow))
                    .is_none_or(|snapshot| snapshot.phase == crate::flow::Phase::Settling)
        })
    }

    pub(super) fn validate_retirements(&self) -> Result<(), Error> {
        let mut retained = 0;
        let mut residual = Counts::ZERO;
        for (&connection, &state) in &self.detached {
            match (state, self.limits.retirement) {
                (Detached::AwaitingRelease, RetirementPolicy::PhysicalRelease)
                | (Detached::Quiescing { .. }, RetirementPolicy::Bounded { .. }) => {}
                (Detached::Released, _)
                    if self.unavailable.get(&connection) == Some(&Counts::ZERO)
                        || self
                            .ledgers
                            .get(&connection)
                            .is_some_and(|ledger| ledger.metadata_usage().retained_flows == 0) => {}
                (Detached::Retired, RetirementPolicy::Bounded { .. }) => {
                    if !self.generation_quiescent(connection) {
                        return Err(Error::InvalidCheckpoint);
                    }
                    retained += 1;
                    let submitted = self.residual_bound(connection);
                    residual.frames = residual
                        .frames
                        .checked_add(submitted.frames)
                        .ok_or(Error::InvalidCheckpoint)?;
                    residual.encoded_bytes = residual
                        .encoded_bytes
                        .checked_add(submitted.encoded_bytes)
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

    pub(super) fn progress_retirements(&mut self, now: UnixMillis) {
        let released: Vec<_> = self
            .detached
            .iter()
            .filter_map(|(&connection, &state)| (state == Detached::Released).then_some(connection))
            .collect();
        for connection in released {
            self.collect_ledger(connection, now);
        }
        let pending: Vec<_> = self
            .detached
            .iter()
            .filter_map(|(&connection, &state)| match state {
                Detached::Quiescing { not_before } => Some((connection, not_before)),
                Detached::AwaitingRelease | Detached::Released | Detached::Retired => None,
            })
            .collect();
        for (connection, not_before) in pending {
            self.deadlines.set(
                Key::Retirement(connection),
                (now < not_before).then_some(not_before),
            );
            if now < not_before || !self.generation_quiescent(connection) {
                continue;
            }
            let residual = self.residual_bound(connection);
            let usage = self.connection_usage();
            let RetirementPolicy::Bounded {
                generations,
                residual: limit,
                ..
            } = self.limits.retirement
            else {
                continue;
            };
            // Empty generations need no residual slot, but still pass the same
            // revocation, queue-discard and host-grace barrier before collection.
            if residual != Counts::ZERO
                && (usage.retired_generations >= generations
                    || residual.frames > limit.frames.saturating_sub(usage.residual.frames)
                    || residual.encoded_bytes
                        > limit
                            .encoded_bytes
                            .saturating_sub(usage.residual.encoded_bytes))
            {
                self.trace(
                    Some(connection),
                    RelayDecision::RetirementBlocked {
                        usage,
                        residual,
                        generation_limit: generations,
                        residual_limit: limit,
                    },
                    now,
                );
                continue;
            }
            self.detached.insert(connection, Detached::Retired);
            self.checkpointing.retention_changed();
            if self.ledgers.contains_key(&connection) {
                self.checkpointing.changed(connection);
            }
            self.trace(Some(connection), RelayDecision::Retired { residual }, now);
            self.collect_ledger(connection, now);
        }
    }
}
