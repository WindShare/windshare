use super::Checkpoint;
use super::RuntimeStatus;
use super::checkpoint::ConnectionRole;
use super::{
    AdmissionState, Connection, ConnectionRef, Deadlines, Error, Identity, Key, Limits, Observer,
    Output, Record, RelayDecision, Role, ShareLifecycle, ShareRelay, Turn,
};
use crate::{
    clock::UnixMillis,
    flow::ConnectionLedger,
    lifecycle::{Restoration, SessionRestore},
    observation::Journal,
    sessions::{Retirement, RetirementReason},
};
use alloc::{
    collections::{BTreeMap, VecDeque},
    vec::Vec,
};

#[derive(Clone, Copy, Debug)]
pub struct Restore<'a> {
    pub record: Option<Record>,
    /// Only these exact connection generations can still communicate.
    pub surviving: &'a [ConnectionRef],
    pub responsibility: Responsibility<'a>,
}

/// Host evidence about all previous generations, independent of socket discovery.
#[derive(Clone, Copy, Debug)]
pub enum Responsibility<'a> {
    /// The latest complete image, including generations with no surviving socket.
    Checkpoint(&'a Checkpoint),
    /// The host proves that all prior application queues and transport buffers
    /// are released (or that this share has never had any). Socket disappearance,
    /// an empty attachment list and a runtime restart are not such evidence.
    Released,
    /// Recovery cannot establish the old responsibility. Refuse new admission.
    Unknown,
}

impl<'a> Restore<'a> {
    #[must_use]
    pub const fn restarted(record: Option<Record>, responsibility: Responsibility<'a>) -> Self {
        Self {
            record,
            surviving: &[],
            responsibility,
        }
    }

    pub(super) const fn checkpoint(&self) -> Option<&'a Checkpoint> {
        match self.responsibility {
            Responsibility::Checkpoint(checkpoint) => Some(checkpoint),
            Responsibility::Released | Responsibility::Unknown => None,
        }
    }
}

impl<O: Observer> ShareRelay<O> {
    /// Validates complete responsibility independently of surviving socket authority.
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
        if limits.connections == 0
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
        let live: Vec<_> = restored
            .surviving
            .iter()
            .copied()
            .filter(|connection| {
                restored.checkpoint().is_some_and(|checkpoint| {
                    checkpoint.connections.iter().any(|entry| {
                        entry.connection == *connection
                            && matches!(entry.role, ConnectionRole::Established)
                    })
                })
            })
            .collect();
        let mut decisions = Vec::new();
        let (lifecycle, action) = ShareLifecycle::restore(
            identity.relay,
            identity.share,
            Restoration {
                record: restored.record,
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
            connections: BTreeMap::new(),
            ledgers: BTreeMap::new(),
            detached: BTreeMap::new(),
            sessions: BTreeMap::new(),
            routes: BTreeMap::new(),
            deadlines: Deadlines::default(),
            stored: restored.record,
            runtime: RuntimeStatus::Running,
            last_session: restored
                .checkpoint()
                .map_or(0, |checkpoint| checkpoint.last_session),
            last_work: 0,
            tasks: VecDeque::new(),
            stop_barrier: None,
            checkpointing: super::checkpoint_tracking::Checkpointing::Disabled,
            observer,
        };
        relay.flush_lifecycle();
        relay.restore_connections(restored, now)?;
        let mut turn = Turn::default();
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
                connections: relay.connections.len(),
                retained_generations: relay.ledgers.len(),
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
            let ledger = ConnectionLedger::restore(
                self.identity.relay,
                connection,
                self.limits.delivery,
                self.limits.retained_flows_per_connection,
                &snapshot.ledger,
                Journal::default(),
            )
            .map_err(|_| Error::InvalidCheckpoint)?;
            if matches!(snapshot.role, ConnectionRole::Established)
                && ledger.receive_capacity().is_none()
            {
                return Err(Error::InvalidCheckpoint);
            }
            for (flow, _) in ledger.checkpoint_flows() {
                if self
                    .routes
                    .insert((connection, flow.session().wire_id()), flow)
                    .is_some()
                {
                    return Err(Error::InvalidCheckpoint);
                }
            }
            self.deadlines
                .set(Key::Ledger(connection), ledger.deadline());
            let communicating = restored.surviving.contains(&connection);
            self.trace(
                Some(connection),
                RelayDecision::ResponsibilityRestored {
                    communicating,
                    usage: ledger.usage(),
                },
                now,
            );
            if matches!(
                snapshot.role,
                ConnectionRole::Detached(super::Detached::Retired)
            ) && !ledger.queues_discarded()
            {
                return Err(Error::InvalidCheckpoint);
            }
            self.ledgers.insert(connection, ledger);
            if !communicating {
                let state = match snapshot.role {
                    ConnectionRole::Detached(state) => state,
                    _ => self.limits.retirement.detached(now),
                };
                self.detached.insert(connection, state);
            }
            if communicating {
                self.connections.insert(
                    connection,
                    Connection {
                        role: if matches!(snapshot.role, ConnectionRole::Closing) {
                            Role::Closing
                        } else {
                            Role::Established
                        },
                        reservation: None,
                    },
                );
            }
        }
        self.validate_retirements()
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
                        self.deadlines
                            .set(Key::Session(session.key().wire_id()), session.deadline());
                        self.sessions.insert(
                            session.key().wire_id(),
                            AdmissionState {
                                session,
                                flows,
                                deferred: false,
                            },
                        );
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
        let destinations: Vec<_> = self.ledgers.keys().copied().collect();
        for destination in destinations {
            let flows: Vec<_> = self.ledgers[&destination].checkpoint_flows().collect();
            for (flow, open) in flows {
                let valid = self
                    .sessions
                    .get(&flow.session().wire_id())
                    .is_some_and(|entry| {
                        entry
                            .session
                            .check_forward(&self.lifecycle, flow.permit(), now)
                            .is_ok()
                    });
                if !valid || !open {
                    if let Some(ledger) = self.ledgers.get_mut(&destination) {
                        let retirement = Retirement {
                            session: flow.session(),
                            reason: RetirementReason::OwnerUnavailable,
                        };
                        let _ = ledger.retire(retirement, now);
                    }
                    turn.outputs.push(Output::Discard(flow));
                }
            }
            self.flush_ledger(destination, now, turn);
            self.collect_ledger(destination, now);
            if self
                .connections
                .get(&destination)
                .is_some_and(|entry| matches!(entry.role, Role::Closing))
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
            self.ledgers
                .get(&destination)?
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
    if restored.surviving.len() > limits.connections {
        return Err(Error::InvalidCheckpoint);
    }
    for (index, connection) in restored.surviving.iter().enumerate() {
        if restored.surviving[..index].contains(connection) {
            return Err(Error::InvalidCheckpoint);
        }
    }
    if let Some(checkpoint) = restored.checkpoint() {
        if checkpoint.identity.relay != identity.relay
            || checkpoint.identity.share != identity.share
            || checkpoint.identity.incarnation == identity.incarnation
        {
            return Err(Error::InvalidCheckpoint);
        }
        checkpoint.validate(limits)?;
        for connection in restored.surviving {
            if !checkpoint.connections.iter().any(|entry| {
                entry.connection == *connection
                    && !matches!(entry.role, ConnectionRole::Detached(_))
            }) {
                return Err(Error::InvalidCheckpoint);
            }
        }
    } else if matches!(restored.responsibility, Responsibility::Unknown) {
        return Err(Error::MissingResponsibility);
    } else if !restored.surviving.is_empty() {
        return Err(Error::InvalidCheckpoint);
    }
    Ok(())
}
