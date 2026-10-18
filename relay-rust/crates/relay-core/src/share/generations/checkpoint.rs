use super::{Connection, ConnectionRef, Delivery, Detached, Generation, Generations, Role};
use crate::share::{
    ConnectionCheckpoint, Error,
    checkpoint::{ConnectionResources, ConnectionRole},
    deadlines::{Deadlines, Key},
    responsibility::{GenerationState, Reservation, ReservationKind},
};
use crate::{
    clock::UnixMillis,
    flow::{ConnectionLedger, Counts},
    handshake::ControlHandshake,
    observation::Journal,
    sessions::{SessionKey, SessionSnapshot},
};
use alloc::{boxed::Box, vec::Vec};

impl Generations {
    /// Manifest reservations deliberately remain separate from exact records:
    /// losing a socket attachment cannot create a fresh admission budget.
    pub(in crate::share) fn reservations(&self) -> Vec<Reservation> {
        self.entries
            .iter()
            .map(|(&connection, entry)| {
                let kind = match entry {
                    Generation::Control { deadline, .. } => ReservationKind::Control {
                        deadline: *deadline,
                    },
                    Generation::Business { .. } => ReservationKind::Business {
                        bound: self.limits.delivery.total(),
                        state: GenerationState::Connected,
                    },
                    Generation::Detached { delivery, state } => {
                        let (bound, state) = match delivery {
                            Delivery::Exact(ledger) => {
                                let bound = match state {
                                    Detached::Retired => ledger.usage().submitted,
                                    Detached::Released => Counts::ZERO,
                                    _ => self.limits.delivery.total(),
                                };
                                (bound, GenerationState::Detached(*state))
                            }
                            Delivery::Unavailable(bound) => {
                                (*bound, GenerationState::Unavailable(*state))
                            }
                        };
                        ReservationKind::Business { bound, state }
                    }
                };
                Reservation { connection, kind }
            })
            .collect()
    }

    pub(in crate::share) fn checkpoint_connection(
        &self,
        connection: ConnectionRef,
        mut session: impl FnMut(SessionKey) -> Option<SessionSnapshot>,
    ) -> Result<ConnectionCheckpoint, Error> {
        let entry = self
            .entries
            .get(&connection)
            .ok_or(Error::UnknownConnection)?;
        let resources = if let Generation::Control { deadline, .. } = entry {
            ConnectionResources::Control {
                deadline: *deadline,
            }
        } else {
            let ledger = entry.ledger().ok_or(Error::UnknownConnection)?;
            let sessions = ledger
                .checkpoint_flows()
                .filter_map(|(flow, _)| {
                    let key = flow.session();
                    (key.receiver() == connection)
                        .then(|| session(key))
                        .flatten()
                })
                .collect();
            ConnectionResources::Business {
                ledger: Box::new(ledger.checkpoint().map_err(|_| Error::CheckpointBusy)?),
                sessions,
            }
        };
        let role = match entry {
            Generation::Control { socket, .. } | Generation::Business { socket, .. } => {
                match &socket.role {
                    Role::Closing => ConnectionRole::Closing,
                    Role::Stopping => ConnectionRole::Stopping,
                    Role::Established(liveness) => ConnectionRole::Established(*liveness),
                    Role::Handshake { transcript, .. } | Role::Admitting { transcript, .. } => {
                        ConnectionRole::Handshake(transcript.clone())
                    }
                }
            }
            Generation::Detached { state, .. } => ConnectionRole::Detached(*state),
        };
        Ok(ConnectionCheckpoint {
            identity: self.identity,
            connection,
            role,
            resources,
        })
    }

    /// Restore one whole generation before exposing it to share/session recovery.
    pub(in crate::share) fn restore_record(
        &mut self,
        snapshot: &ConnectionCheckpoint,
        survives: bool,
        now: UnixMillis,
        deadlines: &mut Deadlines,
    ) -> Result<(), Error> {
        let connection = snapshot.connection;
        if self.contains(&connection) {
            return Err(Error::InvalidCheckpoint);
        }
        let entry = match &snapshot.resources {
            ConnectionResources::Control { deadline } => {
                if !survives {
                    return Ok(());
                }
                let socket = self
                    .restore_socket(connection, &snapshot.role, deadlines)
                    .ok_or(Error::InvalidCheckpoint)?;
                deadlines.set(Key::Control(connection), Some(*deadline));
                Generation::Control {
                    socket,
                    deadline: *deadline,
                }
            }
            ConnectionResources::Business { ledger: saved, .. } => {
                let ledger = ConnectionLedger::restore(
                    self.identity.relay,
                    connection,
                    self.limits.delivery,
                    self.limits.retained_flows_per_connection,
                    saved,
                    Journal::default(),
                )
                .map_err(|_| Error::InvalidCheckpoint)?;
                if matches!(snapshot.role, ConnectionRole::Established(_))
                    && ledger.receive_capacity().is_none()
                    || matches!(snapshot.role, ConnectionRole::Detached(Detached::Retired))
                        && !ledger.queues_discarded()
                {
                    return Err(Error::InvalidCheckpoint);
                }
                deadlines.set(Key::Ledger(connection), ledger.deadline());
                let ledger = Box::new(ledger);
                if survives
                    && let Some(socket) = self.restore_socket(connection, &snapshot.role, deadlines)
                {
                    Generation::Business { socket, ledger }
                } else {
                    let state = match snapshot.role {
                        ConnectionRole::Detached(state) => state,
                        _ => self.limits.retirement.detached(now),
                    };
                    Generation::Detached {
                        delivery: Delivery::Exact(ledger),
                        state,
                    }
                }
            }
        };
        self.entries.insert(connection, entry);
        Ok(())
    }

    pub(in crate::share) fn restore_missing(
        &mut self,
        reservation: Reservation,
        now: UnixMillis,
    ) -> bool {
        if self.contains(&reservation.connection) {
            return false;
        }
        let ReservationKind::Business { bound, state } = reservation.kind else {
            // A missing control attachment never becomes payload responsibility.
            return false;
        };
        let state = match state {
            GenerationState::Connected => self.limits.retirement.detached(now),
            GenerationState::Detached(state) | GenerationState::Unavailable(state) => state,
        };
        self.entries.insert(
            reservation.connection,
            Generation::Detached {
                delivery: Delivery::Unavailable(bound),
                state,
            },
        );
        true
    }

    fn restore_socket(
        &self,
        connection: ConnectionRef,
        role: &ConnectionRole,
        deadlines: &mut Deadlines,
    ) -> Option<Connection> {
        let role = match role {
            ConnectionRole::Established(liveness) => {
                deadlines.set(Key::Liveness(connection), Some(liveness.deadline()));
                Role::Established(*liveness)
            }
            ConnectionRole::Handshake(transcript) => Role::Handshake {
                handshake: Box::new(ControlHandshake::open(
                    self.identity.relay,
                    connection,
                    transcript.opened,
                )),
                transcript: transcript.clone(),
            },
            ConnectionRole::Closing => Role::Closing,
            ConnectionRole::Stopping => Role::Stopping,
            ConnectionRole::Detached(_) => return None,
        };
        Some(Connection {
            role,
            reservation: None,
        })
    }
}
