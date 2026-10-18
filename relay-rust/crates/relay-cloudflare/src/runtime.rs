use crate::config;
use anyhow::{Result, anyhow, bail};
use std::collections::VecDeque;
use windshare_relay_core::{
    challenge::ChallengeSeed,
    clock::UnixMillis,
    error_frame::ErrorFrame,
    lifecycle::{ConnectionRef, Record},
    observation::Observer,
    registration::{RegisterInit, RegistrationMode},
    share::{
        Authority, Checkpoint, CheckpointTarget, Control, Identity, Input, Output, Responsibility,
        Restore, ShareRelay, Storage, Turn, WorkResult,
    },
};

/// Synchronous operations stay in one DO event, behind Cloudflare's output gate.
/// No payload queues or awaited I/O can interleave with the kernel's decisions.
pub trait Host {
    fn now(&self) -> UnixMillis;
    fn challenge(&mut self) -> Result<ChallengeSeed>;
    fn execute(&mut self, operation: &Storage) -> Result<()>;
    fn send(&mut self, connection: ConnectionRef, bytes: &[u8]) -> Result<()>;
    fn close(&mut self, connection: ConnectionRef) -> Result<()>;
    fn checkpoint(&mut self, target: CheckpointTarget, bytes: &[u8]) -> Result<()>;
}

pub struct Runtime<O> {
    relay: ShareRelay<O>,
}

impl<O: Observer> Runtime<O> {
    /// # Errors
    /// Refuses unknown authority or inventory; all recovery work finishes before ingress.
    pub fn restore(
        host: &mut impl Host,
        identity: Identity,
        record: Option<Record>,
        checkpoint: Option<&Checkpoint>,
        surviving: &[ConnectionRef],
        observer: O,
    ) -> Result<Self> {
        let restored = Restore {
            authority: Authority::Known(record),
            surviving,
            // The manifest owns loss deadlines even when every socket/attachment
            // disappears. Only an absent inventory in this lifecycle-only schema
            // can establish that no prior delivery responsibility exists.
            responsibility: match checkpoint {
                Some(checkpoint) => Responsibility::Checkpoint(checkpoint),
                None if surviving.is_empty() => Responsibility::Released { previous: None },
                None => Responsibility::Unknown,
            },
        };
        let (mut relay, turn) =
            ShareRelay::restore(identity, config::limits(), &restored, observer, host.now())
                .map_err(|error| anyhow!("restore refused: {error:?}"))?;
        relay
            .enable_checkpoints(config::budget(), host.now())
            .map_err(|error| anyhow!("checkpoint profile: {error:?}"))?;
        let mut runtime = Self { relay };
        runtime.outputs(host, turn)?;
        runtime.flush(host)?;
        Ok(runtime)
    }

    /// # Errors
    /// I/O uncertainty requires authoritative recovery before another input is accepted.
    pub fn event(&mut self, host: &mut impl Host, input: Input<'_>) -> Result<()> {
        let connection = match &input {
            Input::Control { connection, .. } => Some(*connection),
            _ => None,
        };
        let turn = self.relay.handle(input, host.now());
        let refused = turn.refusal.is_some();
        self.outputs(host, turn)?;
        if refused && let Some(connection) = connection {
            host.close(connection)?;
            let turn = self
                .relay
                .handle(Input::Disconnected(connection), host.now());
            self.outputs(host, turn)?;
        }
        self.flush(host)
    }

    /// # Errors
    /// Unknown frames and uncredentialed fresh registration close only this connection.
    pub fn control(
        &mut self,
        host: &mut impl Host,
        connection: ConnectionRef,
        bytes: &[u8],
        may_register: bool,
    ) -> Result<()> {
        let supported = bytes.len() <= config::MAX_CONTROL_BYTES
            && matches!(
                bytes.get(..4),
                Some(b"WS2R" | b"WS2P" | b"WS2U" | b"WS2T" | b"WS2X" | b"WS2V" | b"WS2H")
            );
        let unauthorized = !may_register
            && RegisterInit::decode(bytes).is_ok_and(|init| init.mode() == RegistrationMode::Fresh);
        if !supported || unauthorized {
            // Deployment permission is independent of signed share ownership.
            // RESUME and STOP remain available when credentials rotate or disappear.
            let refusal = if unauthorized {
                ErrorFrame::InvalidProof
            } else {
                ErrorFrame::UnsupportedMode
            };
            let _ = host.send(connection, &refusal.encode());
            host.close(connection)?;
            return self.event(host, Input::Disconnected(connection));
        }
        self.event(
            host,
            Input::Control {
                connection,
                frame: bytes,
            },
        )
    }

    #[must_use]
    pub fn deadline(&self) -> Option<UnixMillis> {
        self.relay.deadline()
    }

    fn outputs(&mut self, host: &mut impl Host, turn: Turn) -> Result<()> {
        let mut outputs: VecDeque<_> = turn.outputs.into();
        while let Some(output) = outputs.pop_front() {
            let follow = match output {
                Output::ChallengeNeeded(request) => Some(self.relay.handle(
                    Input::Challenge {
                        request,
                        seed: host.challenge()?,
                    },
                    host.now(),
                )),
                Output::Work(work) => {
                    // SQLite's synchronous write is coupled to the output gate;
                    // a failed durability commit cannot leak REGISTERED or STOPPED.
                    host.execute(&work.operation)?;
                    Some(self.relay.handle(
                        Input::Completed {
                            id: work.id,
                            result: WorkResult::Complete,
                        },
                        host.now(),
                    ))
                }
                Output::Send {
                    connection,
                    control,
                } => {
                    if host.send(connection, &encode(control)?).is_err() {
                        host.close(connection)?;
                        Some(
                            self.relay
                                .handle(Input::Disconnected(connection), host.now()),
                        )
                    } else {
                        None
                    }
                }
                Output::Close(connection) => {
                    host.close(connection)?;
                    // This is communication loss, never a physical release receipt.
                    Some(
                        self.relay
                            .handle(Input::Disconnected(connection), host.now()),
                    )
                }
                Output::Discard(flow) => {
                    Some(self.relay.handle(Input::Discarded(flow), host.now()))
                }
                Output::Retry(_) | Output::Reload => bail!("authoritative recovery required"),
                Output::Received(_) => bail!("payload reached lifecycle-only host"),
            };
            if let Some(turn) = follow {
                outputs.extend(turn.outputs);
            }
        }
        Ok(())
    }

    fn flush<H: Host>(&mut self, host: &mut H) -> Result<()> {
        let flush = |relay: &mut ShareRelay<O>, host: &mut H| {
            relay.flush_checkpoints(host.now(), |target, bytes| host.checkpoint(target, bytes))
        };
        if flush(&mut self.relay, host).is_err() {
            // A partial batch remains dirty in the kernel. Retry the complete
            // replacement before yielding, then fail closed if storage still fails.
            flush(&mut self.relay, host)
                .map_err(|error| anyhow!("checkpoint failed: {error:?}"))?;
        }
        Ok(())
    }
}

fn encode(control: Control) -> Result<Vec<u8>> {
    Ok(match control {
        Control::Challenge(bytes) => bytes,
        Control::Error(frame) => frame.encode().to_vec(),
        Control::Registered(frame) => frame.encode().to_vec(),
        Control::Stopped(frame) => frame.encode().to_vec(),
        Control::ProbeAck(frame) => frame.encode().to_vec(),
        Control::Descriptor { .. } | Control::Grant(_) | Control::Settlement(_) => {
            bail!("delivery control reached lifecycle-only host")
        }
    })
}
