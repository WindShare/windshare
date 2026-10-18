use super::{
    Checkpoint, CheckpointManifest, ConnectionCheckpoint, ConnectionRef, Detached, Error, Identity,
    Limits,
};
use super::{
    checkpoint::{ConnectionResources, ConnectionRole},
    handshake_checkpoint::HandshakeCheckpoint,
};
use crate::{
    clock::UnixMillis,
    flow::LedgerCheckpoint,
    heartbeat::Liveness,
    identifiers::{RelayIdentity, ShareId},
    lifecycle::Incarnation,
    sessions::SessionSnapshot,
    wire::Reader,
};
use alloc::{boxed::Box, vec::Vec};

const FORMAT: u8 = 9;
const CONNECTION_FORMAT: u8 = 7;
const LENGTH_BYTES: usize = size_of::<u64>();
const IDENTITY_BYTES: usize = 32 + 12 + 16;
const CONNECTION_HEADER_BYTES: usize =
    1 + IDENTITY_BYTES + ConnectionRef::ENCODED_BYTES + 2 + 2 * LENGTH_BYTES;

impl ConnectionCheckpoint {
    #[must_use]
    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::new();
        out.push(CONNECTION_FORMAT);
        put_identity(&mut out, self.identity);
        out.extend_from_slice(&self.connection.encode());
        match &self.role {
            ConnectionRole::Established(liveness) => {
                out.push(0);
                out.extend_from_slice(&liveness.encode());
            }
            ConnectionRole::Closing => out.push(1),
            ConnectionRole::Stopping => out.push(7),
            ConnectionRole::Detached(state) => {
                out.push(3);
                put_detached(&mut out, *state);
            }
            ConnectionRole::Handshake(handshake) => {
                out.push(2);
                block(&mut out, &handshake.encode());
            }
        }
        match &self.resources {
            ConnectionResources::Control { deadline } => {
                out.push(0);
                out.extend_from_slice(&deadline.0.to_be_bytes());
            }
            ConnectionResources::Business { ledger, sessions } => {
                out.push(1);
                block(&mut out, &ledger.encode());
                out.extend_from_slice(&(sessions.len() as u64).to_be_bytes());
                for session in sessions {
                    block(&mut out, session.encode().as_bytes());
                }
            }
        }
        out
    }

    /// Maximum binary size for this configured retained-flow bound.
    /// A handshake cannot own flows/admissions; established sockets may retain
    /// the full sender fanout, including retired delivery obligations.
    #[must_use]
    pub fn maximum_bytes(retained_flow_limit: usize) -> Option<usize> {
        let established = LedgerCheckpoint::maximum_bytes(retained_flow_limit)?
            .checked_add(
                retained_flow_limit
                    .checked_mul(LENGTH_BYTES + crate::sessions::MAX_SNAPSHOT_BYTES)?,
            )?
            .checked_add(Liveness::ENCODED_BYTES)?;
        let handshake = LedgerCheckpoint::maximum_bytes(0)?
            .checked_add(LENGTH_BYTES + HandshakeCheckpoint::MAX_ENCODED_BYTES)?;
        let detached = LedgerCheckpoint::maximum_bytes(retained_flow_limit)?
            .checked_add(1 + 2 * LENGTH_BYTES)?;
        CONNECTION_HEADER_BYTES.checked_add(established.max(handshake).max(detached))
    }

    /// # Errors
    /// Rejects oversized, malformed or misassigned records before recovery authorizes them.
    pub fn decode(bytes: &[u8], limits: Limits) -> Result<Self, Error> {
        let maximum = Self::maximum_bytes(limits.retained_flows_per_connection)
            .ok_or(Error::InvalidCheckpoint)?;
        if bytes.len() > maximum {
            return Err(Error::InvalidCheckpoint);
        }
        let mut body = Reader::new(bytes);
        if field::<1>(&mut body)? != [CONNECTION_FORMAT] {
            return Err(Error::InvalidCheckpoint);
        }
        let identity = read_identity(&mut body)?;
        let connection =
            ConnectionRef::decode(field(&mut body)?).ok_or(Error::InvalidCheckpoint)?;
        let role = match field::<1>(&mut body)? {
            [0] => ConnectionRole::Established(
                Liveness::decode(&field::<{ Liveness::ENCODED_BYTES }>(&mut body)?)
                    .map_err(|_| Error::InvalidCheckpoint)?,
            ),
            [1] => ConnectionRole::Closing,
            [7] => ConnectionRole::Stopping,
            [3] => ConnectionRole::Detached(read_detached(&mut body)?),
            [2] => ConnectionRole::Handshake(HandshakeCheckpoint::decode(read_block(
                &mut body,
                HandshakeCheckpoint::MAX_ENCODED_BYTES,
            )?)?),
            _ => return Err(Error::InvalidCheckpoint),
        };
        let resources = match field::<1>(&mut body)? {
            [0] => ConnectionResources::Control {
                deadline: crate::clock::UnixMillis(u64::from_be_bytes(field(&mut body)?)),
            },
            [1] => {
                let maximum = LedgerCheckpoint::maximum_bytes(limits.retained_flows_per_connection)
                    .ok_or(Error::InvalidCheckpoint)?;
                let ledger = LedgerCheckpoint::decode(
                    read_block(&mut body, maximum)?,
                    limits.retained_flows_per_connection,
                )
                .map_err(|_| Error::InvalidCheckpoint)?;
                let count = bounded_count(&mut body, limits.sessions)?;
                let mut sessions = Vec::with_capacity(count);
                for _ in 0..count {
                    sessions.push(
                        SessionSnapshot::decode(read_block(
                            &mut body,
                            crate::sessions::MAX_SNAPSHOT_BYTES,
                        )?)
                        .map_err(|_| Error::InvalidCheckpoint)?,
                    );
                }
                ConnectionResources::Business {
                    ledger: Box::new(ledger),
                    sessions,
                }
            }
            _ => return Err(Error::InvalidCheckpoint),
        };
        body.finish().map_err(|_| Error::InvalidCheckpoint)?;
        let checkpoint = Self {
            identity,
            connection,
            role,
            resources,
        };
        checkpoint.validate(limits)?;
        Ok(checkpoint)
    }
}

impl Checkpoint {
    #[must_use]
    pub fn encode(&self) -> Vec<u8> {
        let mut out = Vec::new();
        out.push(FORMAT);
        block(&mut out, &self.manifest.encode());
        out.extend_from_slice(&(self.connections.len() as u64).to_be_bytes());
        for connection in &self.connections {
            block(&mut out, &connection.encode());
        }
        out
    }

    /// # Errors
    /// Rejects malformed, mixed or oversized collections. Restore additionally checks authority.
    pub fn decode(bytes: &[u8], limits: Limits) -> Result<Self, Error> {
        let mut body = Reader::new(bytes);
        if field::<1>(&mut body)? != [FORMAT] {
            return Err(Error::InvalidCheckpoint);
        }
        let manifest = CheckpointManifest::decode(read_block(&mut body, bytes.len())?, limits)?;
        let count = bounded_count(&mut body, limits.generation_limit()?)?;
        let mut connections = Vec::with_capacity(count);
        let maximum = ConnectionCheckpoint::maximum_bytes(limits.retained_flows_per_connection)
            .ok_or(Error::InvalidCheckpoint)?;
        for _ in 0..count {
            connections.push(ConnectionCheckpoint::decode(
                read_block(&mut body, maximum)?,
                limits,
            )?);
        }
        body.finish().map_err(|_| Error::InvalidCheckpoint)?;
        let checkpoint = Self {
            manifest,
            connections,
        };
        checkpoint.validate(limits)?;
        Ok(checkpoint)
    }
}

pub(super) fn put_header(out: &mut Vec<u8>, identity: Identity, last_session: u64) {
    put_identity(out, identity);
    out.extend_from_slice(&last_session.to_be_bytes());
}

// Both records describe the same state; a missing attachment must not change
// the meaning or origin of the manifest's absolute retirement deadlines.
pub(super) fn put_detached(out: &mut Vec<u8>, state: Detached) {
    match state {
        Detached::AwaitingRelease => out.push(0),
        Detached::Released => out.push(1),
        Detached::Quiescing {
            retire_at,
            risk_expires_at,
        } => {
            out.push(2);
            out.extend_from_slice(&retire_at.0.to_be_bytes());
            out.extend_from_slice(&risk_expires_at.0.to_be_bytes());
        }
        Detached::RiskRetained { expires_at } => {
            out.push(3);
            out.extend_from_slice(&expires_at.0.to_be_bytes());
        }
    }
}

pub(super) fn read_detached(body: &mut Reader<'_>) -> Result<Detached, Error> {
    match field::<1>(body)? {
        [0] => Ok(Detached::AwaitingRelease),
        [1] => Ok(Detached::Released),
        [2] => Ok(Detached::Quiescing {
            retire_at: UnixMillis(u64::from_be_bytes(field(body)?)),
            risk_expires_at: UnixMillis(u64::from_be_bytes(field(body)?)),
        }),
        [3] => Ok(Detached::RiskRetained {
            expires_at: UnixMillis(u64::from_be_bytes(field(body)?)),
        }),
        _ => Err(Error::InvalidCheckpoint),
    }
}
fn put_identity(out: &mut Vec<u8>, identity: Identity) {
    out.extend_from_slice(identity.relay.as_bytes());
    out.extend_from_slice(identity.share.as_bytes());
    out.extend_from_slice(identity.incarnation.as_bytes());
}
pub(super) fn read_header(body: &mut Reader<'_>) -> Result<(Identity, u64), Error> {
    Ok((read_identity(body)?, u64::from_be_bytes(field(body)?)))
}
fn read_identity(body: &mut Reader<'_>) -> Result<Identity, Error> {
    let relay = RelayIdentity::new(field(body)?).ok_or(Error::InvalidCheckpoint)?;
    let share = ShareId::new(field(body)?).ok_or(Error::InvalidCheckpoint)?;
    let incarnation = Incarnation::new(field(body)?).ok_or(Error::InvalidCheckpoint)?;
    Ok(Identity {
        relay,
        share,
        incarnation,
    })
}
fn block(out: &mut Vec<u8>, bytes: &[u8]) {
    out.extend_from_slice(&(bytes.len() as u64).to_be_bytes());
    out.extend_from_slice(bytes);
}
pub(super) fn field<const N: usize>(body: &mut Reader<'_>) -> Result<[u8; N], Error> {
    body.array().map_err(|_| Error::InvalidCheckpoint)
}
pub(super) fn bounded_count(body: &mut Reader<'_>, maximum: usize) -> Result<usize, Error> {
    let value =
        usize::try_from(u64::from_be_bytes(field(body)?)).map_err(|_| Error::InvalidCheckpoint)?;
    if value > maximum || value > body.remainder().len() {
        return Err(Error::InvalidCheckpoint);
    }
    Ok(value)
}
fn read_block<'a>(body: &mut Reader<'a>, maximum: usize) -> Result<&'a [u8], Error> {
    let length = bounded_count(body, maximum)?;
    body.take(length).map_err(|_| Error::InvalidCheckpoint)
}
