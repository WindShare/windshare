//! Inert, connection-owned recovery records. Storage layout does not confer authority.
use super::{
    ConnectionRef, Error, Identity, Observer, Role, RuntimeStatus, ShareRelay,
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

/// One socket's complete attachment, including receiver-owned admissions.
///
/// The small share header is replicated so losing any socket cannot lose the
/// wire-ID high-water mark. Admission changes refresh every surviving attachment;
/// ordinary delivery changes only the affected connection.
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
    /// Assemble independent attachments before the kernel's joint authority/accounting check.
    ///
    /// # Errors
    /// Rejects empty, duplicate, foreign or mixed-header collections. With no surviving
    /// sockets use `Restore::restarted`; a missing attachment never means an empty ledger.
    pub fn from_connections(
        connections: Vec<ConnectionCheckpoint>,
        limits: super::Limits,
    ) -> Result<Self, Error> {
        let first = connections.first().ok_or(Error::InvalidCheckpoint)?;
        let checkpoint = Self {
            identity: first.identity,
            last_session: first.last_session,
            connections,
        };
        checkpoint.validate(limits)?;
        Ok(checkpoint)
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
            || self.stop_retirement.is_some()
        {
            return Err(Error::CheckpointBusy);
        }
        Ok(())
    }

    /// Capture only this connection's ledger and admissions, without scanning sibling sessions.
    ///
    /// # Errors
    /// Rejects unknown connections, unfinished durable work and unsubmitted payloads.
    pub fn checkpoint_connection(
        &self,
        connection: ConnectionRef,
    ) -> Result<ConnectionCheckpoint, Error> {
        self.checkpoint_ready()?;
        let entry = self
            .connections
            .get(&connection)
            .ok_or(Error::UnknownConnection)?;
        let sessions = entry
            .ledger
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
            role: match &entry.role {
                Role::Closing => ConnectionRole::Closing,
                Role::Established => ConnectionRole::Established,
                Role::Handshake { transcript, .. } | Role::Admitting { transcript, .. } => {
                    ConnectionRole::Handshake(transcript.clone())
                }
            },
            ledger: entry
                .ledger
                .checkpoint()
                .map_err(|_| Error::CheckpointBusy)?,
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
                .connections
                .keys()
                .map(|&connection| self.checkpoint_connection(connection))
                .collect::<Result<_, _>>()?,
        })
    }
}
