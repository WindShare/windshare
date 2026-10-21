use super::Checkpoint;
use super::RuntimeStatus;
use super::checkpoint::ConnectionRole;
use super::{
    ConnectionPhase, ConnectionRef, Error, Identity, Limits, Observer, Output, Record,
    RelayDecision, ShareLifecycle, ShareRelay, Turn,
};
use crate::{
    clock::UnixMillis,
    lifecycle::{Restoration, SessionRestore},
    observation::Journal,
    sessions::{Retirement, RetirementReason},
};
use alloc::{collections::VecDeque, vec::Vec};

#[derive(Clone, Copy, Debug)]
// This cold-path input already carries Record by value; keep absence/uncertainty
// allocation-free instead of boxing every successful authority read.
#[allow(clippy::large_enum_variant)]
pub enum Authority {
    /// A completed authoritative read; None proves the share has no stored record.
    Known(Option<Record>),
    /// Ownership or permanent STOP may have committed; no delivery record can repair this.
    Unknown,
}

#[derive(Clone, Copy, Debug)]
pub struct Restore<'a> {
    pub authority: Authority,
    /// Only these exact connection generations can still communicate.
    pub surviving: &'a [ConnectionRef],
    pub responsibility: Responsibility<'a>,
}

/// Host evidence about all previous generations, independent of socket discovery.
#[derive(Clone, Copy, Debug)]
pub enum Responsibility<'a> {
    /// An authoritative reservation inventory plus independently validated records.
    /// Missing delivery records are isolated using their retained risk reservation.
    Checkpoint(&'a Checkpoint),
    /// The host proves that all prior application queues and transport buffers
    /// are released (or that this share has never had any). Socket disappearance,
    /// an empty attachment list and a runtime restart are not such evidence.
    Released {
        /// Preserve available admission history: release does not replenish
        /// attempt tokens or reset the session-ID fence.
        previous: Option<&'a super::CheckpointManifest>,
    },
    /// Recovery cannot establish the old responsibility. Refuse new admission.
    Unknown,
}

impl<'a> Restore<'a> {
    #[must_use]
    pub const fn restarted(record: Option<Record>, responsibility: Responsibility<'a>) -> Self {
        Self {
            authority: Authority::Known(record),
            surviving: &[],
            responsibility,
        }
    }

    pub(super) const fn manifest(&self) -> Option<&'a super::CheckpointManifest> {
        match self.responsibility {
            Responsibility::Checkpoint(checkpoint) => Some(&checkpoint.manifest),
            Responsibility::Released { previous } => previous,
            Responsibility::Unknown => None,
        }
    }

    fn established(&self) -> Vec<ConnectionRef> {
        self.surviving
            .iter()
            .copied()
            .filter(|connection| {
                self.checkpoint().is_some_and(|checkpoint| {
                    checkpoint.connections.iter().any(|entry| {
                        entry.connection == *connection
                            && matches!(entry.role, ConnectionRole::Established(_))
                    })
                })
            })
            .collect()
    }

    pub(super) const fn checkpoint(&self) -> Option<&'a Checkpoint> {
        match self.responsibility {
            Responsibility::Checkpoint(checkpoint) => Some(checkpoint),
            Responsibility::Released { .. } | Responsibility::Unknown => None,
        }
    }
}

impl<O: Observer> ShareRelay<O> {
    /// Restores authority independently of delivery-record availability.
    ///
    /// # Errors
    /// Missing responsibility or invalid checkpoints never authorize sockets or
    /// fresh budgets. The host must retain recovery evidence even if it closes
    /// surviving sockets. A fresh incarnation fences old storage completions.
    pub fn restore(
        identity: Identity,
        limits: Limits,
        restored: &Restore<'_>,
        observer: O,
        now: UnixMillis,
    ) -> Result<(Self, Turn), Error> {
        let Authority::Known(record) = restored.authority else {
            return Err(Error::MissingAuthority);
        };
        if limits.business_connections == 0
            || limits.handshakes.concurrent == 0
            || limits.sessions == 0
            || limits.retained_flows_per_connection == 0
            || limits.generation_limit().is_err()
        {
            return Err(Error::Configuration);
        }
        validate_restore(identity, limits, restored)?;
        let snapshots: Vec<_> = restored
            .checkpoint()
            .into_iter()
            .flat_map(Checkpoint::sessions)
            .copied()
            .collect();
        let live = restored.established();
        let mut decisions = Vec::new();
        let (lifecycle, action) = ShareLifecycle::restore(
            identity.relay,
            identity.share,
            Restoration {
                record,
                connections: &live,
                sessions: &snapshots,
            },
            identity.incarnation,
            Journal::default(),
            now,
            |_, decision| decisions.push(decision),
        )
        .map_err(Error::Restore)?;
        let mut relay = Self {
            identity,
            limits,
            lifecycle,
            generations: super::Generations::new(
                identity,
                limits,
                match restored.manifest() {
                    Some(manifest) => super::AttemptLimiter::restore(
                        limits.handshakes.attempts,
                        manifest.attempts,
                    )?,
                    None => super::AttemptLimiter::new(limits.handshakes.attempts, now)?,
                },
            ),
            sessions: super::sessions::Sessions::new(
                restored
                    .manifest()
                    .map_or(0, |manifest| manifest.last_session),
            ),
            stored: record,
            runtime: RuntimeStatus::Running,
            last_work: 0,
            tasks: VecDeque::new(),
            stop_barrier: None,
            checkpointing: super::checkpoint_tracking::Checkpointing::Disabled,
            observer,
        };
        relay.flush_lifecycle();
        relay.restore_connections(restored, now)?;
        let mut turn = Turn::default();
        for &connection in restored.surviving {
            if relay
                .generations
                .connection(&connection)
                .is_none_or(|entry| entry.phase() == ConnectionPhase::Closing)
            {
                turn.outputs.push(Output::Close(connection));
            }
        }
        relay.restore_sessions(decisions, now, &mut turn);
        relay.action(action, None, now, &mut turn);
        relay.restore_handshakes(restored, now, &mut turn)?;
        relay.progress_retirements(now);
        relay.expire(now, &mut turn);
        relay.progress_stop(now, &mut turn);
        relay.start_work(now, &mut turn);
        turn.deadline = relay.deadline();
        relay.trace(
            None,
            RelayDecision::Restored {
                connections: relay.generations.connected_count(),
                retained_generations: relay.generations.retained_count(),
            },
            now,
        );
        Ok((relay, turn))
    }

    fn restore_connections(
        &mut self,
        restored: &Restore<'_>,
        now: UnixMillis,
    ) -> Result<(), Error> {
        let Some(checkpoint) = restored.checkpoint() else {
            return Ok(());
        };
        for snapshot in &checkpoint.connections {
            let connection = snapshot.connection;
            self.generations.restore_record(
                snapshot,
                restored.surviving.contains(&connection),
                now,
            )?;
            if let Some(ledger) = self.generations.ledger(&connection) {
                self.sessions
                    .restore_routes(connection, ledger.checkpoint_flows().map(|(flow, _)| flow))?;
                let usage = ledger.usage();
                let communicating = self.generations.connection(&connection).is_some();
                self.trace(
                    Some(connection),
                    RelayDecision::ResponsibilityRestored {
                        communicating,
                        usage,
                    },
                    now,
                );
            }
        }
        for &reservation in &checkpoint.manifest.reservations {
            if self.generations.restore_missing(reservation, now) {
                let reserved = self
                    .generations
                    .unavailable_reservation(reservation.connection)
                    .expect("restored conservative reservation");
                self.trace(
                    Some(reservation.connection),
                    RelayDecision::RecoveryIsolated { reserved },
                    now,
                );
            }
        }
        self.generations
            .validate_retirements(|connection| self.generation_quiescent(connection))
    }

    fn restore_sessions(
        &mut self,
        decisions: Vec<SessionRestore>,
        now: UnixMillis,
        turn: &mut Turn,
    ) {
        for decision in decisions {
            match decision {
                SessionRestore::Restored(session) => {
                    if let Some(flows) = self.session_flows(session.key()) {
                        self.sessions.install(session, &flows);
                    } else {
                        self.close(session.key().receiver(), turn);
                        self.trace(
                            Some(session.key().receiver()),
                            RelayDecision::InvalidSnapshot,
                            now,
                        );
                    }
                }
                SessionRestore::Retired(retirement) => {
                    self.close(retirement.session.receiver(), turn);
                }
            }
        }
        let destinations: Vec<_> = self.generations.ledger_ids().collect();
        for destination in destinations {
            let flows: Vec<_> = self
                .generations
                .ledger(&destination)
                .unwrap()
                .checkpoint_flows()
                .collect();
            for (flow, open) in flows {
                let valid = self
                    .sessions
                    .entries()
                    .get(&flow.session().wire_id())
                    .is_some_and(|entry| {
                        entry
                            .session
                            .check_forward(&self.lifecycle, flow.permit(), now)
                            .is_ok()
                    });
                if !valid || !open {
                    let _ = self.generations.update_ledger(
                        destination,
                        |ledger| {
                            ledger.retire(
                                Retirement {
                                    session: flow.session(),
                                    reason: RetirementReason::OwnerUnavailable,
                                },
                                now,
                            )
                        },
                        &mut self.observer,
                    );
                    turn.outputs.push(Output::Discard(flow));
                }
            }
            self.apply_delivery_close(destination, now, turn);
            self.collect_ledger(destination, now);
            if self
                .generations
                .connection(&destination)
                .is_some_and(|entry| entry.phase() == ConnectionPhase::Closing)
                && !turn
                    .outputs
                    .iter()
                    .any(|output| matches!(output, Output::Close(value) if *value == destination))
            {
                turn.outputs.push(Output::Close(destination));
            }
        }
    }

    fn session_flows(&self, key: crate::sessions::SessionKey) -> Option<[crate::flow::FlowKey; 2]> {
        let find = |destination| {
            self.generations
                .ledger(&destination)?
                .checkpoint_flows()
                .find_map(|(flow, open)| (flow.session() == key && open).then_some(flow))
        };
        Some([find(key.sender())?, find(key.receiver())?])
    }
}

fn validate_restore(
    identity: Identity,
    limits: Limits,
    restored: &Restore<'_>,
) -> Result<(), Error> {
    if restored.surviving.len() > limits.business_connections + limits.handshakes.concurrent {
        return Err(Error::InvalidCheckpoint);
    }
    for (index, connection) in restored.surviving.iter().enumerate() {
        if restored.surviving[..index].contains(connection) {
            return Err(Error::InvalidCheckpoint);
        }
    }
    if let Some(manifest) = restored.manifest() {
        if manifest.identity.relay != identity.relay
            || manifest.identity.share != identity.share
            || manifest.identity.incarnation == identity.incarnation
        {
            return Err(Error::InvalidCheckpoint);
        }
        manifest.validate(limits)?;
    }
    if let Some(checkpoint) = restored.checkpoint() {
        checkpoint.validate(limits)?;
    } else if matches!(restored.responsibility, Responsibility::Unknown) {
        return Err(Error::MissingResponsibility);
    } else if !restored.surviving.is_empty() {
        return Err(Error::InvalidCheckpoint);
    }
    Ok(())
}
