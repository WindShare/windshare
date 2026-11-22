//! Control state is never mutably borrowed by the share coordinator. Every
//! protocol transition updates its expiry index here.
use super::{Generation, Generations};
use crate::{
    access::Attribution,
    challenge::ChallengeSeed,
    clock::UnixMillis,
    handshake::{Admission, ControlHandshake, Outcome, Step},
    heartbeat::{ConnectionProbe, Liveness},
    identifiers::RelayIdentity,
    lifecycle::{ConnectionRef, OperationId},
    share::{
        Error,
        deadlines::{Deadlines, Key},
    },
};
use alloc::boxed::Box;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(in crate::share) enum Phase {
    AwaitingFirstFrame,
    Handshaking,
    Admitting,
    Established,
    Stopping,
    Closing,
}

#[derive(Debug)]
enum Role {
    Handshake {
        handshake: Box<ControlHandshake>,
        fresh_attribution: Option<Attribution>,
    },
    Admitting {
        handshake: Box<ControlHandshake>,
        admission: Box<Admission>,
    },
    Established(Liveness),
    Stopping,
    Closing,
}

#[derive(Debug)]
pub(in crate::share) struct Connection {
    role: Role,
    reservation: Option<OperationId>,
}

impl Connection {
    pub(super) fn open(relay: RelayIdentity, connection: ConnectionRef, now: UnixMillis) -> Self {
        Self {
            role: Role::Handshake {
                handshake: Box::new(ControlHandshake::open(relay, connection, now)),
                fresh_attribution: None,
            },
            reservation: None,
        }
    }

    #[cfg(feature = "hibernation")]
    pub(super) const fn established(liveness: Liveness) -> Self {
        Self {
            role: Role::Established(liveness),
            reservation: None,
        }
    }

    pub fn phase(&self) -> Phase {
        match &self.role {
            Role::Handshake { handshake, .. }
                if handshake.phase() == crate::handshake::Phase::AwaitingFirstFrame =>
            {
                Phase::AwaitingFirstFrame
            }
            Role::Handshake { .. } => Phase::Handshaking,
            Role::Admitting { .. } => Phase::Admitting,
            Role::Established(_) => Phase::Established,
            Role::Stopping => Phase::Stopping,
            Role::Closing => Phase::Closing,
        }
    }

    pub const fn reservation(&self) -> Option<OperationId> {
        self.reservation
    }

    pub const fn liveness(&self) -> Option<Liveness> {
        match self.role {
            Role::Established(liveness) => Some(liveness),
            _ => None,
        }
    }

    pub fn handshake_deadline(&self) -> Option<UnixMillis> {
        match &self.role {
            Role::Handshake { handshake, .. } | Role::Admitting { handshake, .. } => {
                handshake.deadline()
            }
            _ => None,
        }
    }

    pub const fn fresh_attribution(&self) -> Option<Attribution> {
        match &self.role {
            Role::Handshake {
                fresh_attribution, ..
            } => *fresh_attribution,
            _ => None,
        }
    }

    pub(super) fn synchronize_deadlines(
        &self,
        connection: ConnectionRef,
        deadlines: &mut Deadlines,
    ) {
        deadlines.set(Key::Handshake(connection), self.handshake_deadline());
        deadlines.set(
            Key::Liveness(connection),
            self.liveness().map(Liveness::deadline),
        );
        if self.phase() == Phase::Closing {
            deadlines.set(Key::Control(connection), None);
        }
    }

    fn complete(&mut self, outcome: &Outcome<'_>, now: UnixMillis) {
        self.role = if matches!(outcome, Outcome::Stop(_)) {
            Role::Stopping
        } else {
            Role::Established(Liveness::established(now))
        };
    }
}

impl Generations {
    pub(in crate::share) fn bind_registration(
        &mut self,
        connection: ConnectionRef,
        attribution: Attribution,
    ) -> Result<(), Error> {
        let socket = self
            .entries
            .get_mut(&connection)
            .and_then(Generation::socket_mut)
            .ok_or(Error::UnknownConnection)?;
        if socket.phase() != Phase::AwaitingFirstFrame {
            return Err(Error::Unavailable);
        }
        let Role::Handshake {
            fresh_attribution, ..
        } = &mut socket.role
        else {
            unreachable!("first frame requires a handshake");
        };
        *fresh_attribution = Some(attribution);
        Ok(())
    }

    pub(in crate::share) fn receive_handshake<'f>(
        &mut self,
        connection: ConnectionRef,
        frame: &'f [u8],
        now: UnixMillis,
    ) -> Result<Step<'f>, Error> {
        let socket = self
            .entries
            .get_mut(&connection)
            .and_then(Generation::socket_mut)
            .ok_or(Error::UnknownConnection)?;
        let Role::Handshake { handshake, .. } = &mut socket.role else {
            return Err(Error::Unavailable);
        };
        let step = handshake.receive(frame, now);
        if let Step::Complete(outcome) = &step {
            socket.complete(outcome, now);
        }
        socket.synchronize_deadlines(connection, &mut self.deadlines);
        Ok(step)
    }

    pub(in crate::share) fn offer_admission(
        &mut self,
        connection: ConnectionRef,
        admission: Admission,
    ) -> bool {
        let Some(socket) = self
            .entries
            .get_mut(&connection)
            .and_then(Generation::socket_mut)
        else {
            return false;
        };
        let role = core::mem::replace(&mut socket.role, Role::Closing);
        let Role::Handshake { handshake, .. } = role else {
            socket.role = role;
            return false;
        };
        if let Admission::Fresh(reservation) = &admission {
            socket.reservation = Some(reservation.id());
        }
        socket.role = Role::Admitting {
            handshake,
            admission: Box::new(admission),
        };
        socket.synchronize_deadlines(connection, &mut self.deadlines);
        true
    }

    pub(in crate::share) fn challenge(
        &mut self,
        connection: ConnectionRef,
        seed: ChallengeSeed,
        now: UnixMillis,
    ) -> Result<Step<'static>, Error> {
        let socket = self
            .entries
            .get_mut(&connection)
            .and_then(Generation::socket_mut)
            .ok_or(Error::UnknownConnection)?;
        let role = core::mem::replace(&mut socket.role, Role::Closing);
        let Role::Admitting {
            mut handshake,
            admission,
        } = role
        else {
            socket.role = role;
            return Err(Error::Unavailable);
        };
        let step = handshake.admit(*admission, seed, now);
        socket.role = Role::Handshake {
            handshake,
            fresh_attribution: None,
        };
        if let Step::Complete(outcome) = &step {
            socket.complete(outcome, now);
        }
        socket.synchronize_deadlines(connection, &mut self.deadlines);
        Ok(step)
    }

    pub(in crate::share) fn registration_committed(&mut self, connection: ConnectionRef) -> bool {
        let Some(socket) = self
            .entries
            .get_mut(&connection)
            .and_then(Generation::socket_mut)
        else {
            return false;
        };
        socket.reservation = None;
        true
    }

    pub(in crate::share) fn accept_probe(
        &mut self,
        connection: ConnectionRef,
        probe: ConnectionProbe,
        now: UnixMillis,
    ) -> Result<(), Error> {
        let socket = self
            .entries
            .get_mut(&connection)
            .and_then(Generation::socket_mut)
            .ok_or(Error::Unavailable)?;
        let Role::Established(liveness) = &mut socket.role else {
            return Err(Error::Unavailable);
        };
        if !liveness.accept_probe(probe) {
            return Err(Error::ProbeNonce);
        }
        liveness.accepted_inbound(now);
        self.deadlines
            .set(Key::Liveness(connection), Some(liveness.deadline()));
        Ok(())
    }

    pub(in crate::share) fn accepted_inbound(
        &mut self,
        connection: ConnectionRef,
        now: UnixMillis,
    ) {
        if let Some(socket) = self
            .entries
            .get_mut(&connection)
            .and_then(Generation::socket_mut)
            && let Role::Established(liveness) = &mut socket.role
            && liveness.accepted_inbound(now)
        {
            self.deadlines
                .set(Key::Liveness(connection), Some(liveness.deadline()));
        }
    }

    pub(in crate::share) fn close(&mut self, connection: ConnectionRef) -> bool {
        let Some(socket) = self
            .entries
            .get_mut(&connection)
            .and_then(Generation::socket_mut)
        else {
            return false;
        };
        if socket.phase() == Phase::Closing {
            return false;
        }
        socket.role = Role::Closing;
        socket.synchronize_deadlines(connection, &mut self.deadlines);
        true
    }
}
