//! Control state is never mutably borrowed by the share coordinator. Every
//! protocol transition updates its recovery projection and expiry index here.
use super::{Generation, Generations};
use crate::{
    access::Attribution,
    challenge::ChallengeSeed,
    clock::UnixMillis,
    handshake::{Admission, ControlHandshake, Outcome, Step},
    heartbeat::{ConnectionProbe, Liveness},
    identifiers::RelayIdentity,
    lifecycle::{ConnectionRef, OperationId, Record},
    share::{
        Error,
        checkpoint::ConnectionRole,
        deadlines::{Deadlines, Key},
        handshake_checkpoint::{self, Binding, HandshakeCheckpoint},
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
        transcript: HandshakeCheckpoint,
    },
    Admitting {
        handshake: Box<ControlHandshake>,
        admission: Box<Admission>,
        transcript: HandshakeCheckpoint,
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
                transcript: HandshakeCheckpoint::new(now),
            },
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
            Role::Handshake { transcript, .. } => match transcript.binding {
                Binding::Fresh(attribution) => Some(attribution),
                _ => None,
            },
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

    pub(super) fn checkpoint(&self) -> ConnectionRole {
        match &self.role {
            Role::Handshake { transcript, .. } | Role::Admitting { transcript, .. } => {
                ConnectionRole::Handshake(transcript.clone())
            }
            Role::Established(liveness) => ConnectionRole::Established(*liveness),
            Role::Stopping => ConnectionRole::Stopping,
            Role::Closing => ConnectionRole::Closing,
        }
    }

    pub(super) fn restore(
        relay: RelayIdentity,
        connection: ConnectionRef,
        role: &ConnectionRole,
    ) -> Option<Self> {
        let role = match role {
            ConnectionRole::Handshake(transcript) => Role::Handshake {
                handshake: Box::new(ControlHandshake::open(relay, connection, transcript.opened)),
                transcript: transcript.clone(),
            },
            ConnectionRole::Established(liveness) => Role::Established(*liveness),
            ConnectionRole::Stopping => Role::Stopping,
            ConnectionRole::Closing => Role::Closing,
            ConnectionRole::Detached(_) => return None,
        };
        Some(Self {
            role,
            reservation: None,
        })
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
        let Role::Handshake { transcript, .. } = &mut socket.role else {
            unreachable!("first frame requires a handshake");
        };
        transcript.binding = Binding::Fresh(attribution);
        self.changes.changed(connection);
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
        let Role::Handshake {
            handshake,
            transcript,
        } = &mut socket.role
        else {
            return Err(Error::Unavailable);
        };
        let step = handshake.receive(frame, now);
        let recorded = if handshake.phase() == crate::handshake::Phase::Finished {
            Ok(())
        } else {
            transcript.frame(now, frame)
        };
        if let Step::Complete(outcome) = &step {
            socket.complete(outcome, now);
        }
        // A failed transcript append still follows a protocol mutation.
        self.changes.changed(connection);
        socket.synchronize_deadlines(connection, &mut self.deadlines);
        recorded?;
        Ok(step)
    }

    pub(in crate::share) fn offer_admission(
        &mut self,
        connection: ConnectionRef,
        admission: Admission,
        ownership: Option<Record>,
    ) -> bool {
        let Some(socket) = self
            .entries
            .get_mut(&connection)
            .and_then(Generation::socket_mut)
        else {
            return false;
        };
        let role = core::mem::replace(&mut socket.role, Role::Closing);
        let Role::Handshake {
            handshake,
            mut transcript,
        } = role
        else {
            socket.role = role;
            return false;
        };
        match &admission {
            Admission::Fresh(reservation) => socket.reservation = Some(reservation.id()),
            Admission::Resume(_) => {
                transcript.binding = Binding::Resume(handshake_checkpoint::ownership(ownership));
            }
            Admission::Stop => {}
        }
        socket.role = Role::Admitting {
            handshake,
            admission: Box::new(admission),
            transcript,
        };
        self.changes.changed(connection);
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
        if let Role::Admitting { transcript, .. } = &mut socket.role {
            transcript.challenge(now, seed.checkpoint())?;
        }
        let role = core::mem::replace(&mut socket.role, Role::Closing);
        let Role::Admitting {
            mut handshake,
            admission,
            transcript,
        } = role
        else {
            socket.role = role;
            return Err(Error::Unavailable);
        };
        let step = handshake.admit(*admission, seed, now);
        socket.role = Role::Handshake {
            handshake,
            transcript,
        };
        if let Step::Complete(outcome) = &step {
            socket.complete(outcome, now);
        }
        self.changes.changed(connection);
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
        // Reservation identity is reconstructed by replay, not an independent record.
        socket.reservation = None;
        true
    }

    pub(in crate::share) fn replay_handshake(
        &mut self,
        connection: ConnectionRef,
        transcript: &HandshakeCheckpoint,
    ) -> Result<(), Error> {
        let socket = self
            .entries
            .get_mut(&connection)
            .and_then(Generation::socket_mut)
            .ok_or(Error::InvalidCheckpoint)?;
        *socket = Connection::open(self.identity.relay, connection, transcript.opened);
        let Role::Handshake {
            transcript: replay, ..
        } = &mut socket.role
        else {
            unreachable!("opened handshake");
        };
        replay.binding = transcript.binding;
        self.changes.changed(connection);
        socket.synchronize_deadlines(connection, &mut self.deadlines);
        Ok(())
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
        self.changes.changed(connection);
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
            self.changes.changed(connection);
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
        self.changes.changed(connection);
        socket.synchronize_deadlines(connection, &mut self.deadlines);
        true
    }
}
