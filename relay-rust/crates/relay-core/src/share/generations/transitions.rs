use super::{Connection, ConnectionRef, Delivery, Detached, Generation, Generations, Role};
use crate::share::{
    Error,
    checkpoint_tracking::Checkpointing,
    deadlines::{Deadlines, Key},
    handshake_checkpoint::HandshakeCheckpoint,
};
use crate::{
    clock::UnixMillis,
    flow::{ConnectionLedger, Counts},
    handshake::ControlHandshake,
    observation::Journal,
    sessions::SessionKey,
};
use alloc::{boxed::Box, vec::Vec};

impl Generations {
    pub(in crate::share) fn check_control_admission(
        &self,
        connection: ConnectionRef,
    ) -> Result<(), Error> {
        if self.contains(&connection) {
            return Err(Error::UnknownConnection);
        }
        if self.usage().controls >= self.limits.handshakes.concurrent {
            return Err(Error::Capacity);
        }
        Ok(())
    }

    pub(in crate::share) fn connect(
        &mut self,
        connection: ConnectionRef,
        now: UnixMillis,
        checkpointing: &mut Checkpointing,
        deadlines: &mut Deadlines,
    ) -> Result<(), Error> {
        self.check_control_admission(connection)?;
        let handshake = ControlHandshake::open(self.identity.relay, connection, now);
        deadlines.set(Key::Handshake(connection), handshake.deadline());
        let deadline = now.saturating_add(crate::share::admission::CONTROL_LIFETIME);
        deadlines.set(Key::Control(connection), Some(deadline));
        self.entries.insert(
            connection,
            Generation::Control {
                socket: Connection {
                    role: Role::Handshake {
                        handshake: Box::new(handshake),
                        transcript: HandshakeCheckpoint::new(now),
                    },
                    reservation: None,
                },
                deadline,
            },
        );
        checkpointing.inserted(connection);
        Ok(())
    }

    pub(in crate::share) fn admit_business(
        &mut self,
        connection: ConnectionRef,
        checkpointing: &mut Checkpointing,
        deadlines: &mut Deadlines,
    ) -> Result<(), Error> {
        match self.entries.get(&connection) {
            Some(Generation::Business { .. }) => return Ok(()),
            Some(Generation::Control { .. }) => {}
            _ => return Err(Error::UnknownConnection),
        }
        if self.usage().business >= self.limits.business_connections {
            return Err(Error::Capacity);
        }
        let ledger = ConnectionLedger::new(
            self.identity.relay,
            connection,
            self.limits.delivery,
            self.limits.retained_flows_per_connection,
            Journal::default(),
        )
        .map_err(|_| Error::Configuration)?;
        let Some(Generation::Control { socket, .. }) = self.entries.remove(&connection) else {
            unreachable!("validated control generation")
        };
        self.entries.insert(
            connection,
            Generation::Business {
                socket,
                ledger: Box::new(ledger),
            },
        );
        deadlines.set(Key::Control(connection), None);
        checkpointing.changed(connection);
        checkpointing.retention_changed();
        Ok(())
    }

    pub(in crate::share) fn close(
        &mut self,
        connection: ConnectionRef,
        checkpointing: &mut Checkpointing,
        deadlines: &mut Deadlines,
    ) -> bool {
        let Some(socket) = self.connection_mut(&connection) else {
            return false;
        };
        if matches!(socket.role, Role::Closing) {
            return false;
        }
        socket.role = Role::Closing;
        checkpointing.changed(connection);
        clear_socket_deadlines(connection, deadlines);
        true
    }

    /// The share revokes authority before detaching. Only control admission can
    /// disappear here: a business generation keeps its exact delivery ledger.
    pub(in crate::share) fn detach(
        &mut self,
        connection: ConnectionRef,
        now: UnixMillis,
        checkpointing: &mut Checkpointing,
        deadlines: &mut Deadlines,
    ) {
        let Some(entry) = self.entries.remove(&connection) else {
            return;
        };
        clear_socket_deadlines(connection, deadlines);
        match entry {
            Generation::Control { .. } => checkpointing.removed(connection),
            Generation::Business { ledger, .. } => {
                self.entries.insert(
                    connection,
                    Generation::Detached {
                        delivery: Delivery::Exact(ledger),
                        state: self.limits.retirement.detached(now),
                    },
                );
                checkpointing.changed(connection);
                checkpointing.retention_changed();
            }
            detached @ Generation::Detached { .. } => {
                // Duplicate loss reports cannot restart the fixed grace.
                self.entries.insert(connection, detached);
            }
        }
    }

    /// Physical release affects only this destination. The caller still joins
    /// cleanup of queues sourced here and held by other destinations.
    pub(in crate::share) fn release(
        &mut self,
        connection: ConnectionRef,
        now: UnixMillis,
        checkpointing: &mut Checkpointing,
        deadlines: &mut Deadlines,
        observer: &mut impl crate::observation::Observer,
    ) -> Vec<SessionKey> {
        let Some(Generation::Detached { delivery, state }) = self.entries.get_mut(&connection)
        else {
            return Vec::new();
        };
        let affected = match delivery {
            Delivery::Exact(ledger) => {
                let affected = ledger
                    .checkpoint_flows()
                    .map(|(flow, _)| flow.session())
                    .collect();
                let _ = ledger.confirm_closed(connection, now);
                for event in ledger.observer_mut().0.drain(..) {
                    observer.observe(event);
                }
                checkpointing.changed(connection);
                affected
            }
            Delivery::Unavailable(bound) => {
                *bound = Counts::ZERO;
                Vec::new()
            }
        };
        *state = Detached::Released;
        checkpointing.retention_changed();
        deadlines.set(Key::Retirement(connection), None);
        deadlines.set(Key::Ledger(connection), None);
        affected
    }

    /// Called after cross-generation queue quiescence. Empty accounting alone
    /// never authorizes collection of a communicating or unretired generation.
    pub(in crate::share) fn collect(
        &mut self,
        connection: ConnectionRef,
        checkpointing: &mut Checkpointing,
        deadlines: &mut Deadlines,
    ) -> bool {
        let Some(Generation::Detached {
            delivery,
            state: Detached::Released | Detached::RiskRetained { .. },
        }) = self.entries.get(&connection)
        else {
            return false;
        };
        let empty = match delivery {
            Delivery::Exact(ledger) => ledger.metadata_usage().retained_flows == 0,
            Delivery::Unavailable(bound) => *bound == Counts::ZERO,
        };
        if !empty {
            return false;
        }
        self.remove_detached(connection, checkpointing, deadlines);
        true
    }

    pub(super) fn remove_detached(
        &mut self,
        connection: ConnectionRef,
        checkpointing: &mut Checkpointing,
        deadlines: &mut Deadlines,
    ) {
        self.entries.remove(&connection);
        deadlines.set(Key::Retirement(connection), None);
        deadlines.set(Key::Ledger(connection), None);
        checkpointing.removed(connection);
    }
}

fn clear_socket_deadlines(connection: ConnectionRef, deadlines: &mut Deadlines) {
    deadlines.set(Key::Handshake(connection), None);
    deadlines.set(Key::Control(connection), None);
    deadlines.set(Key::Liveness(connection), None);
}
