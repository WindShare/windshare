//! Inert generation recovery records. Storage layout does not confer authority.
use super::{
    CheckpointManifest, ConnectionRef, Error, Identity, Observer, Role, RuntimeStatus, ShareRelay,
    handshake_checkpoint::HandshakeCheckpoint,
};
use crate::{flow::LedgerCheckpoint, sessions::SessionSnapshot};
use alloc::vec::Vec;

/// Complete metadata for recovery/testing; hot-path hosts use connection checkpoints.
#[derive(Clone, Debug)]
pub struct Checkpoint {
    pub(super) identity: Identity,
    pub(super) last_session: u64,
    pub(super) connections: Vec<ConnectionCheckpoint>,
}

/// One generation's complete responsibility, including receiver-owned admissions.
///
/// The small share header is replicated so losing any socket cannot lose the
/// wire-ID high-water mark. Admission changes refresh every retained record;
/// ordinary delivery changes only the affected generation. Detached generations
/// must remain recoverable independently of their former socket's attachment.
#[derive(Clone, Debug)]
pub struct ConnectionCheckpoint {
    pub(super) identity: Identity,
    pub(super) last_session: u64,
    pub(super) connection: ConnectionRef,
    pub(super) role: ConnectionRole,
    pub(super) ledger: LedgerCheckpoint,
    pub(super) sessions: Vec<SessionSnapshot>,
}

#[derive(Clone, Debug)]
pub(super) enum ConnectionRole {
    Established,
    Closing,
    Detached,
    Handshake(HandshakeCheckpoint),
}

/// Host serialization allowance, independent of any platform's numeric limits.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CheckpointBudget {
    payload_bytes: usize,
}

impl CheckpointBudget {
    /// Reserve space for the host's structured serialization and own metadata.
    #[must_use]
    pub const fn new(attachment_bytes: usize, envelope_bytes: usize) -> Option<Self> {
        match attachment_bytes.checked_sub(envelope_bytes) {
            Some(payload_bytes) if payload_bytes > 0 => Some(Self { payload_bytes }),
            _ => None,
        }
    }

    #[must_use]
    pub const fn payload_bytes(self) -> usize {
        self.payload_bytes
    }

    /// Check the worst retained state, not only the current (possibly empty) ledger.
    ///
    /// # Errors
    /// Refuses bounds that could exceed this host's attachment allowance.
    pub fn check(self, session_limit: usize) -> Result<(), Error> {
        if ConnectionCheckpoint::maximum_bytes(session_limit)
            .is_some_and(|maximum| maximum <= self.payload_bytes)
        {
            Ok(())
        } else {
            Err(Error::CheckpointCapacity)
        }
    }
}

impl ConnectionCheckpoint {
    #[must_use]
    pub const fn connection(&self) -> ConnectionRef {
        self.connection
    }

    pub(super) fn validate(&self, limits: super::Limits) -> Result<(), Error> {
        if self.connection != self.ledger.destination()
            || self.sessions.len() > limits.sessions
            || self.ledger.flows().any(|flow| {
                u64::from_be_bytes(*flow.session().wire_id().as_bytes()) > self.last_session
            })
            || self.sessions.iter().any(|session| {
                session.key().receiver() != self.connection
                    || u64::from_be_bytes(*session.key().wire_id().as_bytes()) > self.last_session
            })
            || matches!(self.role, ConnectionRole::Handshake(_))
                && (!self.sessions.is_empty() || self.ledger.flows().next().is_some())
        {
            return Err(Error::InvalidCheckpoint);
        }
        Ok(())
    }
}

impl Checkpoint {
    /// Assemble records against an independently retained, complete inventory.
    ///
    /// # Errors
    /// Rejects missing, unexpected, duplicate, foreign or mixed-header generations.
    /// The host must recover the latest manifest independently of socket discovery.
    pub fn from_connections(
        manifest: &CheckpointManifest,
        mut connections: Vec<ConnectionCheckpoint>,
        limits: super::Limits,
    ) -> Result<Self, Error> {
        manifest.validate(limits)?;
        connections.sort_unstable_by_key(|entry| entry.connection);
        if !manifest
            .generations()
            .iter()
            .copied()
            .eq(connections.iter().map(|entry| entry.connection))
        {
            return Err(Error::MissingResponsibility);
        }
        let checkpoint = Self {
            identity: manifest.identity,
            last_session: manifest.last_session,
            connections,
        };
        checkpoint.validate(limits)?;
        Ok(checkpoint)
    }

    #[must_use]
    pub fn manifest(&self) -> CheckpointManifest {
        let mut generations: Vec<_> = self
            .connections
            .iter()
            .map(|entry| entry.connection)
            .collect();
        generations.sort_unstable();
        CheckpointManifest {
            identity: self.identity,
            last_session: self.last_session,
            generations,
        }
    }

    pub(super) fn sessions(&self) -> impl Iterator<Item = &SessionSnapshot> {
        self.connections.iter().flat_map(|entry| &entry.sessions)
    }

    pub(super) fn validate(&self, limits: super::Limits) -> Result<(), Error> {
        if self.connections.len() > limits.connections || self.sessions().count() > limits.sessions
        {
            return Err(Error::InvalidCheckpoint);
        }
        for (index, entry) in self.connections.iter().enumerate() {
            if entry.identity != self.identity
                || entry.last_session != self.last_session
                || self.connections[..index]
                    .iter()
                    .any(|prior| prior.connection == entry.connection)
            {
                return Err(Error::InvalidCheckpoint);
            }
            entry.validate(limits)?;
        }
        Ok(())
    }
}

impl<O: Observer> ShareRelay<O> {
    pub(super) fn checkpoint_ready(&self) -> Result<(), Error> {
        if self.runtime == RuntimeStatus::ReloadRequired
            || !self.tasks.is_empty()
            || self.stop_barrier.is_some()
        {
            return Err(Error::CheckpointBusy);
        }
        Ok(())
    }

    /// Capture this generation, including when its socket is no longer present.
    ///
    /// # Errors
    /// Rejects unknown connections, unfinished durable work and unsubmitted payloads.
    pub fn checkpoint_connection(
        &self,
        connection: ConnectionRef,
    ) -> Result<ConnectionCheckpoint, Error> {
        self.checkpoint_ready()?;
        let ledger = self
            .ledgers
            .get(&connection)
            .ok_or(Error::UnknownConnection)?;
        let sessions = ledger
            .checkpoint_flows()
            .filter_map(|(flow, _)| {
                let key = flow.session();
                (key.receiver() == connection)
                    .then(|| self.sessions.get(&key.wire_id())?.session.snapshot())
                    .flatten()
            })
            .collect();
        Ok(ConnectionCheckpoint {
            identity: self.identity,
            last_session: self.last_session,
            connection,
            role: match self.connections.get(&connection).map(|entry| &entry.role) {
                None => ConnectionRole::Detached,
                Some(Role::Closing) => ConnectionRole::Closing,
                Some(Role::Established) => ConnectionRole::Established,
                Some(Role::Handshake { transcript, .. } | Role::Admitting { transcript, .. }) => {
                    ConnectionRole::Handshake(transcript.clone())
                }
            },
            ledger: ledger.checkpoint().map_err(|_| Error::CheckpointBusy)?,
            sessions,
        })
    }

    /// Capture a full recovery image. Native forwarding never needs this operation.
    ///
    /// # Errors
    /// Incomplete storage work and unsubmitted payloads must finish before hibernation.
    pub fn checkpoint(&self) -> Result<Checkpoint, Error> {
        self.checkpoint_ready()?;
        Ok(Checkpoint {
            identity: self.identity,
            last_session: self.last_session,
            connections: self
                .ledgers
                .keys()
                .map(|&connection| self.checkpoint_connection(connection))
                .collect::<Result<_, _>>()?,
        })
    }
}
