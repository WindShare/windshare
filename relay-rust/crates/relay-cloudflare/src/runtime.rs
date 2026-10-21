mod delivery;
mod outputs;

use crate::config;
use anyhow::{Result, anyhow};
use windshare_relay_core::{
    authority::StoredAuthority,
    challenge::ChallengeSeed,
    clock::UnixMillis,
    lifecycle::ConnectionRef,
    observation::Observer,
    share::{
        Authority, Checkpoint, CheckpointTarget, Identity, Input, Responsibility, Restore,
        ShareRelay, Storage,
    },
};

/// Synchronous operations stay in one DO event, behind Cloudflare's output gate.
/// No payload queues or awaited I/O can interleave with the kernel's decisions.
pub trait Host {
    fn now(&self) -> UnixMillis;
    fn challenge(&mut self) -> Result<ChallengeSeed>;
    fn execute(&mut self, operation: &Storage) -> Result<Option<StoredAuthority>>;
    fn send(&mut self, connection: ConnectionRef, bytes: &[u8]) -> Result<()>;
    fn close(&mut self, connection: ConnectionRef) -> Result<()>;
    fn checkpoint(&mut self, target: CheckpointTarget, bytes: &[u8]) -> Result<()>;
}

/// Only a completely absent durable identity can establish first creation.
/// An existing identity with missing inventory is a recovery failure, even if
/// authority and all sockets have disappeared.
#[derive(Clone, Copy)]
pub enum Recovery<'a> {
    New,
    Existing {
        authority: Option<&'a StoredAuthority>,
        checkpoint: Option<&'a Checkpoint>,
        surviving: &'a [ConnectionRef],
    },
}

pub struct Runtime<O> {
    relay: ShareRelay<O>,
    authority: Option<StoredAuthority>,
}

impl<O: Observer> Runtime<O> {
    /// # Errors
    /// Refuses unknown authority or inventory; all recovery work finishes before ingress.
    pub fn restore(
        host: &mut impl Host,
        identity: Identity,
        recovery: Recovery<'_>,
        observer: O,
    ) -> Result<Self> {
        let (authority, surviving, responsibility) = match recovery {
            Recovery::New => (None, &[][..], Responsibility::Released { previous: None }),
            Recovery::Existing {
                authority,
                checkpoint,
                surviving,
            } => (
                authority.cloned(),
                surviving,
                checkpoint.map_or(Responsibility::Unknown, Responsibility::Checkpoint),
            ),
        };
        let restored = Restore {
            authority: Authority::Known(authority.as_ref().map(StoredAuthority::record)),
            surviving,
            responsibility,
        };
        let (mut relay, step) =
            ShareRelay::restore(identity, config::limits(), &restored, observer, host.now())
                .map_err(|error| anyhow!("restore refused: {error:?}"))?;
        relay
            .enable_checkpoints(config::budget(), host.now())
            .map_err(|error| anyhow!("checkpoint profile: {error:?}"))?;
        let mut runtime = Self { relay, authority };
        runtime.outputs(host, step)?;
        runtime.flush(host)?;
        Ok(runtime)
    }

    /// # Errors
    /// I/O uncertainty requires authoritative recovery before another input is accepted.
    pub fn event(&mut self, host: &mut impl Host, input: Input<'_>) -> Result<()> {
        let connection = match &input {
            Input::Control { connection, .. } | Input::Unsupported(connection) => Some(*connection),
            Input::Register(grant) => Some(grant.connection()),
            _ => None,
        };
        let step = self.relay.handle(input, host.now());
        let refused = step.refusal.is_some();
        self.outputs(host, step)?;
        if refused && let Some(connection) = connection {
            self.disconnect(host, connection)?;
        }
        self.flush(host)
    }

    #[must_use]
    pub fn deadline(&self) -> Option<UnixMillis> {
        self.relay.deadline()
    }

    fn disconnect(&mut self, host: &mut impl Host, connection: ConnectionRef) -> Result<()> {
        host.close(connection)?;
        let step = self
            .relay
            .handle(Input::Disconnected(connection), host.now());
        self.outputs(host, step)
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
