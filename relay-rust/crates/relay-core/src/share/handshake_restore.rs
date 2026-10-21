use super::generations::Connection;
use super::{
    ChallengeRequest, ConnectionPhase, Error, Output, RelayStep, Restore, ShareRelay,
    checkpoint::ConnectionRole, handshake_checkpoint::Step,
};
use crate::{challenge::ChallengeSeed, clock::UnixMillis, observation::Observer};

impl<O: Observer> ShareRelay<O> {
    pub(super) fn restore_handshakes(
        &mut self,
        restored: &Restore<'_>,
        now: UnixMillis,
        step: &mut RelayStep,
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
                self.disconnect(snapshot.connection, now, step);
                continue;
            }
            let connection = snapshot.connection;
            let declared_capacity = self
                .generations
                .ledger(&connection)
                .and_then(crate::flow::ConnectionLedger::receive_capacity);
            self.generations.replay_handshake(connection, transcript)?;
            let mut replay = RelayStep::default();
            for recorded in &transcript.steps {
                match recorded {
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
                        step.outputs.extend(
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
                step.outputs.push(Output::ChallengeNeeded(ChallengeRequest {
                    connection,
                    incarnation: self.identity.incarnation,
                }));
            }
        }
        Ok(())
    }
}
