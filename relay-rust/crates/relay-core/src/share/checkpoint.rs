//! Inert generation recovery records. Storage layout does not confer authority.
use super::{
    CheckpointManifest, ConnectionRef, Error, Identity, Observer, RuntimeStatus, ShareRelay,
    handshake_checkpoint::HandshakeCheckpoint,
};
use crate::{flow::LedgerCheckpoint, sessions::SessionSnapshot};
use alloc::{boxed::Box, vec::Vec};

/// Complete metadata for recovery/testing; hot-path hosts use connection checkpoints.
#[derive(Clone, Debug)]
pub struct Checkpoint {
    pub(super) manifest: CheckpointManifest,
    pub(super) connections: Vec<ConnectionCheckpoint>,
}

/// One generation's complete responsibility, including receiver-owned admissions.
///
/// Session-ID fencing belongs to the independent manifest, so admitting a new
/// receiver never rewrites unrelated receivers' attachments.
#[derive(Clone, Debug)]
pub struct ConnectionCheckpoint {
    pub(super) identity: Identity,
    pub(super) connection: ConnectionRef,
    pub(super) role: ConnectionRole,
    pub(super) resources: ConnectionResources,
}

#[derive(Clone, Debug)]
pub(super) enum ConnectionResources {
    Control {
        deadline: crate::clock::UnixMillis,
    },
    Business {
        ledger: Box<LedgerCheckpoint>,
        sessions: Vec<SessionSnapshot>,
    },
}

#[derive(Clone, Debug)]
pub(super) enum ConnectionRole {
    Established(crate::heartbeat::Liveness),
    Closing,
    Stopping,
    Detached(super::Detached),
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
    pub fn check(self, retained_flow_limit: usize) -> Result<(), Error> {
        if ConnectionCheckpoint::maximum_bytes(retained_flow_limit)
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

    #[cfg(test)]
    pub(super) fn ledger(&self) -> Option<&LedgerCheckpoint> {
        match &self.resources {
            ConnectionResources::Business { ledger, .. } => Some(ledger),
            ConnectionResources::Control { .. } => None,
        }
    }

    pub(super) fn sessions(&self) -> &[SessionSnapshot] {
        match &self.resources {
            ConnectionResources::Business { sessions, .. } => sessions,
            ConnectionResources::Control { .. } => &[],
        }
    }

    pub(super) fn validate(&self, limits: super::Limits) -> Result<(), Error> {
        if let ConnectionRole::Detached(state) = self.role {
            state.validate(limits.retirement)?;
        }
        if let ConnectionRole::Handshake(transcript) = &self.role {
            let business = transcript.steps.first().is_some_and(|step| {
                matches!(step, super::handshake_checkpoint::Step::Frame { bytes, .. }
                    if crate::handshake::Initiation::decode(bytes)
                        .is_ok_and(|init| init.uses_business_capacity()))
            });
            if business != matches!(self.resources, ConnectionResources::Business { .. }) {
                return Err(Error::InvalidCheckpoint);
            }
        }
        match &self.resources {
            ConnectionResources::Control { deadline } => match &self.role {
                ConnectionRole::Handshake(transcript)
                    if *deadline
                        == transcript
                            .opened
                            .saturating_add(super::admission::CONTROL_LIFETIME) => {}
                ConnectionRole::Closing | ConnectionRole::Stopping => {}
                _ => return Err(Error::InvalidCheckpoint),
            },
            ConnectionResources::Business { ledger, sessions } => {
                if self.connection != ledger.destination()
                    || sessions.len() > limits.sessions
                    || sessions
                        .iter()
                        .any(|session| session.key().receiver() != self.connection)
                    || matches!(self.role, ConnectionRole::Detached(_)) && !sessions.is_empty()
                    || matches!(self.role, ConnectionRole::Stopping)
                    || matches!(self.role, ConnectionRole::Handshake(_))
                        && (!sessions.is_empty() || ledger.flows().next().is_some())
                {
                    return Err(Error::InvalidCheckpoint);
                }
            }
        }
        Ok(())
    }
}

impl Checkpoint {
    /// Recover each record against independently retained generation reservations.
    ///
    /// # Errors
    /// Refuses an invalid manifest. Missing, duplicate or invalid delivery records
    /// remain reserved but cannot authorize a connection or any dependent session.
    /// Hosts omit records that could not be read; they must never substitute stale ones.
    pub fn from_connections(
        manifest: &CheckpointManifest,
        mut connections: Vec<ConnectionCheckpoint>,
        limits: super::Limits,
    ) -> Result<Self, Error> {
        manifest.validate(limits)?;
        connections.sort_unstable_by_key(|entry| entry.connection);
        let mut accepted = Vec::new();
        for reservation in &manifest.reservations {
            let mut candidates = connections
                .iter()
                .filter(|entry| entry.connection == reservation.connection);
            if let Some(entry) = candidates.next()
                && candidates.next().is_none()
                && manifest.accepts(entry, limits)
            {
                accepted.push(entry.clone());
            }
        }
        let checkpoint = Self {
            manifest: manifest.clone(),
            connections: accepted,
        };
        checkpoint.validate(limits)?;
        Ok(checkpoint)
    }

    #[must_use]
    pub fn manifest(&self) -> CheckpointManifest {
        self.manifest.clone()
    }

    pub(super) fn sessions(&self) -> impl Iterator<Item = &SessionSnapshot> {
        self.connections
            .iter()
            .flat_map(ConnectionCheckpoint::sessions)
    }

    pub(super) fn validate(&self, limits: super::Limits) -> Result<(), Error> {
        self.manifest.validate(limits)?;
        if self.sessions().count() > limits.sessions {
            return Err(Error::InvalidCheckpoint);
        }
        for (index, entry) in self.connections.iter().enumerate() {
            if !self.manifest.accepts(entry, limits)
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
        self.generations.checkpoint_connection(connection, |key| {
            self.sessions
                .get(&key.wire_id())
                .and_then(|entry| entry.session.snapshot())
        })
    }

    /// Capture a full recovery image. Native forwarding never needs this operation.
    ///
    /// # Errors
    /// Incomplete storage work and unsubmitted payloads must finish before hibernation.
    pub fn checkpoint(&self) -> Result<Checkpoint, Error> {
        self.checkpoint_ready()?;
        Ok(Checkpoint {
            manifest: self.checkpoint_manifest()?,
            connections: self
                .generations
                .record_ids()
                .map(|connection| self.checkpoint_connection(connection))
                .collect::<Result<_, _>>()?,
        })
    }
}
