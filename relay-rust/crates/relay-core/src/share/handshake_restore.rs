use super::{
    ChallengeRequest, Error, Output, Restore, Role, ShareRelay, Turn,
    deadlines::Key,
    handshake_checkpoint::{HandshakeCheckpoint, Step},
    restoration::ConnectionRole,
};
use crate::{
    challenge::ChallengeSeed, clock::UnixMillis, handshake::ControlHandshake, observation::Observer,
};
use alloc::boxed::Box;

impl<const N: usize, O: Observer> ShareRelay<N, O> {
    pub(super) fn restore_handshakes(
        &mut self,
        restored: &Restore<'_>,
        now: UnixMillis,
        turn: &mut Turn,
    ) -> Result<(), Error> {
        let Some(checkpoint) = restored.checkpoint else {
            return Ok(());
        };
        for snapshot in &checkpoint.connections {
            let ConnectionRole::Handshake(transcript) = &snapshot.role else {
                continue;
            };
            if !restored.surviving.contains(&snapshot.connection) {
                continue;
            }
            if !transcript.valid_at(self.identity, now) {
                return Err(Error::InvalidCheckpoint);
            }
            if !transcript.applicable(self.stored) {
                // A later owner must not become the incumbent of a challenge
                // that was issued against an earlier owner before hibernation.
                self.disconnect(snapshot.connection, now, turn);
                continue;
            }
            let connection = snapshot.connection;
            let entry = self
                .connections
                .get_mut(&connection)
                .ok_or(Error::InvalidCheckpoint)?;
            if entry.ledger.checkpoint_flows().next().is_some() {
                return Err(Error::InvalidCheckpoint);
            }
            let declared_capacity = entry.ledger.receive_capacity();
            let handshake =
                ControlHandshake::open(self.identity.relay, connection, transcript.opened);
            self.deadlines
                .set(Key::Handshake(connection), handshake.deadline());
            entry.role = Role::Handshake {
                handshake: Box::new(handshake),
                transcript: HandshakeCheckpoint::new(transcript.opened),
            };
            let mut replay = Turn::default();
            for step in &transcript.steps {
                match step {
                    Step::Frame { at, bytes } => {
                        self.control(connection, bytes, *at, &mut replay)?;
                    }
                    Step::Challenge { at, bytes } => {
                        let seed = ChallengeSeed::new(
                            *bytes.first_chunk().ok_or(Error::InvalidCheckpoint)?,
                            *bytes.last_chunk().ok_or(Error::InvalidCheckpoint)?,
                        )
                        .ok_or(Error::InvalidCheckpoint)?;
                        self.challenge(connection, seed, *at, &mut replay)?;
                    }
                }
                match self.connections.get(&connection).map(|entry| &entry.role) {
                    Some(Role::Handshake { .. } | Role::Admitting { .. }) => {}
                    Some(Role::Closing) => {
                        turn.outputs.extend(
                            replay
                                .outputs
                                .drain(..)
                                .filter(|output| matches!(output, Output::Close(_))),
                        );
                        break;
                    }
                    _ => return Err(Error::InvalidCheckpoint),
                }
            }
            if self
                .connections
                .get(&connection)
                .and_then(|entry| entry.ledger.receive_capacity())
                != declared_capacity
            {
                return Err(Error::InvalidCheckpoint);
            }
            if matches!(
                self.connections.get(&connection).map(|entry| &entry.role),
                Some(Role::Admitting { .. })
            ) {
                turn.outputs.push(Output::ChallengeNeeded(ChallengeRequest {
                    connection,
                    incarnation: self.identity.incarnation,
                }));
            }
        }
        Ok(())
    }
}
