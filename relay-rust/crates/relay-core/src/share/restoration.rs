use super::Checkpoint;
use super::Readiness;
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
    pub surviving: &'a [ConnectionRef],
    pub checkpoint: Option<&'a Checkpoint>,
}
impl Restore<'static> {
    #[must_use]
    pub const fn restarted(record: Option<Record>) -> Self {
        Self {
            record,
            surviving: &[],
            checkpoint: None,
        }
    }
}

impl<O: Observer> ShareRelay<O> {
    /// Restores lifecycle, then sessions, then delivery responsibility in that order.
    ///
    /// # Errors
    /// Invalid checkpoints never authorize surviving sockets; close those sockets
    /// if restoration fails. A fresh incarnation fences all old host completions.
    pub fn restore(
        identity: Identity,
        limits: Limits,
        restored: &Restore<'_>,
        observer: O,
        now: UnixMillis,
    ) -> Result<(Self, Turn), Error> {
        if limits.connections == 0 || limits.sessions == 0 {
            return Err(Error::Configuration);
        }
        validate_restore(identity, limits, restored)?;
        let snapshots: Vec<_> = restored
            .checkpoint
            .into_iter()
            .flat_map(Checkpoint::sessions)
            .copied()
            .collect();
        let live: Vec<_> = restored
            .surviving
            .iter()
            .copied()
            .filter(|connection| {
                restored.checkpoint.is_some_and(|checkpoint| {
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
            sessions: BTreeMap::new(),
            routes: BTreeMap::new(),
            deadlines: Deadlines::default(),
            stored: restored.record,
            readiness: Readiness::Hidden,
            last_session: restored
                .checkpoint
                .map_or(0, |checkpoint| checkpoint.last_session),
            last_work: 0,
            tasks: VecDeque::new(),
            stop_retirement: None,
            checkpointing: super::checkpoint_tracking::Checkpointing::Disabled,
            observer,
        };
        relay.readiness = if relay.lifecycle.phase() == crate::lifecycle::Phase::Published {
            Readiness::Ready
        } else {
            Readiness::Hidden
        };
        relay.flush_lifecycle();
        relay.restore_connections(restored)?;
        let mut turn = Turn::default();
        relay.restore_sessions(decisions, now, &mut turn);
        relay.action(action, None, now, &mut turn);
        relay.restore_handshakes(restored, now, &mut turn)?;
        relay.expire(now, &mut turn);
        relay.progress_stop(now, &mut turn);
        relay.start_work(now, &mut turn);
        turn.deadline = relay.deadline();
        relay.trace(None, RelayDecision::Restored, now);
        Ok((relay, turn))
    }

    fn restore_connections(&mut self, restored: &Restore<'_>) -> Result<(), Error> {
        for &connection in restored.surviving {
            let snapshot = restored
                .checkpoint
                .and_then(|checkpoint| {
                    checkpoint
                        .connections
                        .iter()
                        .find(|entry| entry.connection == connection)
                })
                .ok_or(Error::InvalidCheckpoint)?;
            let ledger = ConnectionLedger::restore(
                self.identity.relay,
                connection,
                self.limits.delivery,
                self.limits.sessions,
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
            self.connections.insert(
                connection,
                Connection {
                    role: if matches!(snapshot.role, ConnectionRole::Closing) {
                        Role::Closing
                    } else {
                        Role::Established
                    },
                    reservation: None,
                    ledger,
                },
            );
        }
        Ok(())
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
        let destinations: Vec<_> = self.connections.keys().copied().collect();
        for destination in destinations {
            let flows: Vec<_> = self.connections[&destination]
                .ledger
                .checkpoint_flows()
                .collect();
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
                    if let Some(connection) = self.connections.get_mut(&destination) {
                        let retirement = Retirement {
                            session: flow.session(),
                            reason: RetirementReason::OwnerUnavailable,
                        };
                        let _ = connection.ledger.retire(retirement, now);
                    }
                    turn.outputs.push(Output::Discard(flow));
                }
            }
            self.flush_ledger(destination, now, turn);
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
            self.connections
                .get(&destination)?
                .ledger
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
    if let Some(checkpoint) = restored.checkpoint {
        if checkpoint.identity.relay != identity.relay
            || checkpoint.identity.share != identity.share
            || checkpoint.identity.incarnation == identity.incarnation
        {
            return Err(Error::InvalidCheckpoint);
        }
        checkpoint.validate(limits)?;
    } else if !restored.surviving.is_empty() {
        return Err(Error::InvalidCheckpoint);
    }
    Ok(())
}
