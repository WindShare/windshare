mod delivery;
mod outputs;

use crate::config;
use anyhow::{Result, anyhow, ensure};
use std::collections::BTreeMap;
use windshare_relay_core::{
    authority::StoredAuthority,
    challenge::ChallengeSeed,
    clock::UnixMillis,
    lifecycle::ConnectionRef,
    observation::Observer,
    share::{ConnectionSnapshot, Hibernated, Identity, Input, ShareRelay, Storage},
};

/// Synchronous operations stay in one DO event, behind Cloudflare's output gate.
/// No payload queues or awaited I/O can interleave with the kernel's decisions.
pub trait Host {
    fn now(&self) -> UnixMillis;
    fn challenge(&mut self) -> Result<ChallengeSeed>;
    fn execute(&mut self, operation: &Storage) -> Result<Option<StoredAuthority>>;
    fn send(&mut self, connection: ConnectionRef, bytes: &[u8]) -> Result<()>;
    fn close(&mut self, connection: ConnectionRef) -> Result<()>;
    fn save_attachment(&mut self, connection: ConnectionRef, bytes: &[u8]) -> Result<()>;
    fn save_last_session(&mut self, last_session: u64) -> Result<()>;
}

/// Only a completely absent durable identity can establish first creation.
/// Existing Durable Objects restore durable authority, session watermark, and
/// surviving established-connection attachments.
#[derive(Clone, Copy)]
pub enum Recovery<'a> {
    New,
    Existing {
        authority: Option<&'a StoredAuthority>,
        last_session: u64,
        surviving: &'a [ConnectionRef],
        connections: &'a [ConnectionSnapshot],
    },
}

pub struct Runtime<O> {
    relay: ShareRelay<O>,
    authority: Option<StoredAuthority>,
    last_session: u64,
    attachments: BTreeMap<ConnectionRef, Vec<u8>>,
}

impl<O: Observer> Runtime<O> {
    /// # Errors
    /// Refuses invalid storage or snapshot invariants; all recovery work finishes before ingress.
    pub fn restore(
        host: &mut impl Host,
        identity: Identity,
        recovery: Recovery<'_>,
        observer: O,
    ) -> Result<Self> {
        let limits = config::limits();
        config::budget()
            .check(limits.retained_flows_per_connection)
            .map_err(|error| anyhow!("attachment budget: {error:?}"))?;
        let (authority, last_session, (relay, step)) = match recovery {
            Recovery::New => (
                None,
                0,
                ShareRelay::restore(identity, limits, None, observer, host.now())
                    .map_err(|error| anyhow!("restore refused: {error:?}"))?,
            ),
            Recovery::Existing {
                authority,
                last_session,
                surviving,
                connections,
            } => {
                let hibernated = Hibernated {
                    record: authority.map(StoredAuthority::record),
                    last_session,
                    surviving,
                    connections,
                };
                (
                    authority.cloned(),
                    last_session,
                    ShareRelay::restore_hibernated(
                        identity,
                        limits,
                        &hibernated,
                        observer,
                        host.now(),
                    )
                    .map_err(|error| anyhow!("restore refused: {error:?}"))?,
                )
            }
        };
        let mut runtime = Self {
            relay,
            authority,
            last_session,
            attachments: BTreeMap::new(),
        };
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

    fn flush(&mut self, host: &mut impl Host) -> Result<()> {
        if self.try_flush(host).is_err() {
            // Failed writes leave self.attachments and self.last_session stale
            // for uncommitted items; retry once before failing closed.
            self.try_flush(host)?;
        }
        Ok(())
    }

    fn try_flush(&mut self, host: &mut impl Host) -> Result<()> {
        let next_session = self.relay.last_session();
        if next_session > self.last_session {
            host.save_last_session(next_session)?;
            self.last_session = next_session;
        }
        let established: Vec<_> = self.relay.established_connections().collect();
        self.attachments
            .retain(|conn, _| established.contains(conn));
        for connection in established {
            let Some(snapshot) = self
                .relay
                .snapshot_connection(connection)
                .map_err(|error| anyhow!("snapshot refused: {error:?}"))?
            else {
                continue;
            };
            let bytes = snapshot.encode();
            ensure!(
                bytes.len() <= config::budget().payload_bytes(),
                "attachment budget"
            );
            if self.attachments.get(&connection).map(Vec::as_slice) != Some(bytes.as_slice()) {
                host.save_attachment(connection, &bytes)?;
                self.attachments.insert(connection, bytes);
            }
        }
        Ok(())
    }
}
