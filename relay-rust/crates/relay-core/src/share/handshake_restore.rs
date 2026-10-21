use super::generations::Connection;
use super::{
    ChallengeRequest, ConnectionPhase, Error, Output, Restore, ShareRelay, Turn,
    checkpoint::ConnectionRole, handshake_checkpoint::Step,
};
use crate::{challenge::ChallengeSeed, clock::UnixMillis, observation::Observer};

impl<O: Observer> ShareRelay<O> {
    pub(super) fn restore_handshakes(
        &mut self,
        restored: &Restore<'_>,
        now: UnixMillis,
        turn: &mut Turn,
    ) -> Result<(), Error> {
        let Some(checkpoint) = restored.checkpoint() else {
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
            let declared_capacity = self
                .generations
                .ledger(&connection)
                .and_then(crate::flow::ConnectionLedger::receive_capacity);
            self.generations.replay_handshake(connection, transcript)?;
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
                match self
                    .generations
                    .connection(&connection)
                    .map(Connection::phase)
                {
                    Some(
                        ConnectionPhase::AwaitingFirstFrame
                        | ConnectionPhase::Handshaking
                        | ConnectionPhase::Admitting,
                    ) => {}
                    Some(ConnectionPhase::Closing) => {
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
                .generations
                .ledger(&connection)
                .and_then(crate::flow::ConnectionLedger::receive_capacity)
                != declared_capacity
            {
                return Err(Error::InvalidCheckpoint);
            }
            if matches!(
                self.generations
                    .connection(&connection)
                    .map(Connection::phase),
                Some(ConnectionPhase::Admitting)
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
