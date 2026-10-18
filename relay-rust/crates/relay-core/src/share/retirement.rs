//! Share-level retirement coordination: a source can own queued work on other
//! destinations. The generation registry owns local transitions and risk limits.
use super::generations::RetirementProgress;
use super::{ConnectionRef, ConnectionUsage, Error, Limits, Observer, RelayDecision, ShareRelay};
use crate::{clock::UnixMillis, flow::Counts};
use alloc::vec::Vec;

impl Limits {
    pub(super) fn generation_limit(self) -> Result<usize, Error> {
        self.retirement.validate()?;
        self.business_connections
            .checked_add(self.handshakes.concurrent)
            .and_then(|value| value.checked_add(self.retirement.generations()))
            .ok_or(Error::Configuration)
    }
}

impl<O: Observer> ShareRelay<O> {
    #[must_use]
    pub fn connection_usage(&self) -> ConnectionUsage {
        self.generations.usage()
    }

    /// None means exact delivery accounting is available or no responsibility remains.
    #[must_use]
    pub fn unavailable_reservation(&self, connection: ConnectionRef) -> Option<Counts> {
        self.generations.unavailable_reservation(connection)
    }

    pub(super) fn generation_quiescent(&self, connection: ConnectionRef) -> bool {
        if self.sessions.values().any(|entry| {
            let key = entry.session.key();
            (key.sender() == connection || key.receiver() == connection)
                && (entry.deferred || entry.session.phase() != crate::sessions::Phase::Retired)
        }) {
            return false;
        }
        // The lost peer can also source payloads queued on healthy destinations.
        self.routes.values().all(|flow| {
            (flow.destination() != connection && flow.permit().source() != connection)
                || self
                    .generations
                    .ledger(&flow.destination())
                    .and_then(|ledger| ledger.snapshot(*flow))
                    .is_none_or(|snapshot| snapshot.phase == crate::flow::Phase::Settling)
        })
    }

    pub(super) fn progress_retirements(&mut self, now: UnixMillis) {
        let released: Vec<_> = self.generations.released().collect();
        for connection in released {
            self.collect_ledger(connection, now);
        }
        let pending = self.generations.due_retirements(now, &mut self.deadlines);
        for connection in pending {
            if !self.generation_quiescent(connection) {
                continue;
            }
            match self.generations.progress_retirement(
                connection,
                now,
                &mut self.checkpointing,
                &mut self.deadlines,
            ) {
                RetirementProgress::Waiting => {}
                RetirementProgress::Blocked {
                    usage,
                    residual,
                    generation_limit,
                    residual_limit,
                    expires_at,
                } => {
                    self.trace(
                        Some(connection),
                        RelayDecision::RetirementBlocked {
                            usage,
                            residual,
                            generation_limit,
                            residual_limit,
                            expires_at,
                        },
                        now,
                    );
                }
                RetirementProgress::Retired {
                    residual,
                    expires_at,
                } => {
                    self.trace(
                        Some(connection),
                        RelayDecision::Retired {
                            residual,
                            expires_at,
                        },
                        now,
                    );
                    self.collect_ledger(connection, now);
                }
                RetirementProgress::RiskExpired {
                    residual,
                    expires_at,
                } => {
                    self.routes
                        .retain(|(destination, _), _| *destination != connection);
                    self.trace(
                        Some(connection),
                        RelayDecision::RetirementRiskExpired {
                            residual,
                            expires_at,
                        },
                        now,
                    );
                }
            }
        }
    }
}
