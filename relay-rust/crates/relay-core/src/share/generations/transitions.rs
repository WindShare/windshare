use super::{Connection, ConnectionRef, Delivery, Detached, Generation, Generations};
use crate::share::{
    Error,
    deadlines::{Deadlines, Key},
};
use crate::{
    clock::UnixMillis,
    flow::{ConnectionLedger, Counts},
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
    ) -> Result<(), Error> {
        self.check_control_admission(connection)?;
        let previous = self.attempts.state();
        let accepted = self.attempts.attempt(now);
        if self.attempts.state() != previous {
            self.changes.retention_changed();
        }
        if !accepted {
            return Err(Error::HandshakeRate);
        }
        let socket = Connection::open(self.identity.relay, connection, now);
        socket.synchronize_deadlines(connection, &mut self.deadlines);
        let deadline = now.saturating_add(crate::share::admission::CONTROL_LIFETIME);
        self.deadlines.set(Key::Control(connection), Some(deadline));
        self.entries
            .insert(connection, Generation::Control { socket, deadline });
        self.changes.inserted(connection);
        Ok(())
    }

    pub(in crate::share) fn admit_business(
        &mut self,
        connection: ConnectionRef,
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
        self.deadlines.set(Key::Control(connection), None);
        self.changes.changed(connection);
        self.changes.retention_changed();
        Ok(())
    }

    /// The share revokes authority before detaching. Only control admission can
    /// disappear here: a business generation keeps its exact delivery ledger.
    pub(in crate::share) fn detach(&mut self, connection: ConnectionRef, now: UnixMillis) {
        let Some(entry) = self.entries.remove(&connection) else {
            return;
        };
        clear_socket_deadlines(connection, &mut self.deadlines);
        match entry {
            Generation::Control { .. } => self.changes.removed(connection),
            Generation::Business { ledger, .. } => {
                self.entries.insert(
                    connection,
                    Generation::Detached {
                        delivery: Delivery::Exact(ledger),
                        state: self.limits.retirement.detached(now),
                    },
                );
                self.schedule_retirement(connection);
                self.changes.changed(connection);
                self.changes.retention_changed();
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
                self.changes.changed(connection);
                affected
            }
            Delivery::Unavailable(bound) => {
                *bound = Counts::ZERO;
                Vec::new()
            }
        };
        *state = Detached::Released;
        self.changes.retention_changed();
        self.deadlines.set(Key::Retirement(connection), None);
        self.deadlines.set(Key::Ledger(connection), None);
        affected
    }

    /// Called after cross-generation queue quiescence. Empty accounting alone
    /// never authorizes collection of a communicating or unretired generation.
    pub(in crate::share) fn collect(&mut self, connection: ConnectionRef) -> bool {
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
        self.remove_detached(connection);
        true
    }

    pub(super) fn remove_detached(&mut self, connection: ConnectionRef) {
        self.entries.remove(&connection);
        self.deadlines.set(Key::Retirement(connection), None);
        self.deadlines.set(Key::Ledger(connection), None);
        self.changes.removed(connection);
    }
}

pub(super) fn clear_socket_deadlines(connection: ConnectionRef, deadlines: &mut Deadlines) {
    deadlines.set(Key::Handshake(connection), None);
    deadlines.set(Key::Control(connection), None);
    deadlines.set(Key::Liveness(connection), None);
}
